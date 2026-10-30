package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/utmapp/utm-games-db/internal/mail"
)

// LoginRequest is a validated sign-in request waiting for a worker.
type LoginRequest struct {
	Email  string // normalized; discarded once the mail is sent
	IP     string
	Secret string // from the browser's sign-in cookie; the code is bound to it
}

// LoginQueue is the only path on the public site that can spend resources
// before sign-in (an Argon2 hash, a DNS lookup and an SMTP delivery per
// request). Handlers validate cheaply and enqueue; a fixed number of workers
// drain the queue, which caps concurrency and memory (workers x 64 MiB).
// The response to the user never depends on anything the workers do.
type LoginQueue struct {
	Service     *Service
	Mailer      mail.Mailer
	Log         *slog.Logger
	SiteHost    string // host named in the email's domain-bound code line
	CodeTTL     time.Duration
	MXCheck     bool
	LookupMX    func(ctx context.Context, domain string) (bool, error) // nil = net resolver
	MXCacheTTL  time.Duration
	queue       chan LoginRequest
	size        int
	workers     int
	mu          sync.Mutex
	cond        *sync.Cond
	pending     int
	stopped     bool
	wg          sync.WaitGroup
	mx          map[string]mxEntry
	mxMu        sync.Mutex
	droppedFull int64
}

type mxEntry struct {
	ok      bool
	expires time.Time
}

// NewLoginQueue creates a queue; call Start to run the workers.
func NewLoginQueue(size, workers int) *LoginQueue {
	if size < 1 {
		size = 1
	}
	if workers < 1 {
		workers = 1
	}
	q := &LoginQueue{queue: make(chan LoginRequest, size), size: size, workers: workers, mx: map[string]mxEntry{}, MXCacheTTL: time.Hour}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Start launches the workers.
func (q *LoginQueue) Start() {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for req := range q.queue {
				q.process(req)
				q.mu.Lock()
				q.pending--
				q.cond.Broadcast()
				q.mu.Unlock()
			}
		}()
	}
}

// Stop drains the queue and waits for the workers.
func (q *LoginQueue) Stop() {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return
	}
	q.stopped = true
	q.mu.Unlock()
	close(q.queue)
	q.wg.Wait()
}

// Enqueue hands a request to the workers without blocking. It returns false
// when the queue is full; the caller still answers the user as if it were
// accepted, so a flood reveals nothing and costs nothing beyond this check.
func (q *LoginQueue) Enqueue(req LoginRequest) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return false
	}
	select {
	case q.queue <- req:
		q.pending++
		return true
	default:
		q.droppedFull++
		if q.droppedFull == 1 || q.droppedFull%100 == 0 {
			q.Log.Warn("sign-in queue full; dropping requests", "dropped", q.droppedFull)
		}
		return false
	}
}

// Flush blocks until every queued request has been processed (tests).
func (q *LoginQueue) Flush() {
	q.mu.Lock()
	for q.pending > 0 {
		q.cond.Wait()
	}
	q.mu.Unlock()
}

// process performs the expensive part of a sign-in. Nothing here is visible
// to the requester; failures are logged with a hash prefix, never an address.
func (q *LoginQueue) process(req LoginRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	emailHash := q.Service.Keys.HashEmail(req.Email)
	if q.MXCheck {
		ok, err := q.deliverable(ctx, req.Email)
		if err != nil {
			q.Log.Warn("mx lookup failed; allowing", "id", HashPrefix(emailHash), "err", err)
		} else if !ok {
			q.Log.Info("sign-in skipped: domain has no mail servers", "id", HashPrefix(emailHash))
			return
		}
	}
	code, err := q.Service.CreateCode(ctx, req.Secret, emailHash, req.IP)
	if err != nil {
		q.Log.Error("create sign-in code", "id", HashPrefix(emailHash), "err", err)
		return
	}
	if err := q.Mailer.Send(ctx, req.Email, "Your UTM Database sign-in code", CodeBody(code, q.SiteHost, q.CodeTTL)); err != nil {
		q.Log.Error("send sign-in code", "id", HashPrefix(emailHash), "err", redactAddress(err, req.Email))
		return
	}
	q.Log.Info("sign-in code sent", "id", HashPrefix(emailHash))
}

// deliverable reports whether the address's domain can receive mail, with a
// cache (positive and negative) so repeated requests cost no DNS.
func (q *LoginQueue) deliverable(ctx context.Context, email string) (bool, error) {
	_, domain, ok := strings.Cut(email, "@")
	if !ok || domain == "" {
		return false, nil
	}
	domain = strings.ToLower(domain)
	q.mxMu.Lock()
	if e, ok := q.mx[domain]; ok && time.Now().Before(e.expires) {
		q.mxMu.Unlock()
		return e.ok, nil
	}
	q.mxMu.Unlock()
	lookup := q.LookupMX
	if lookup == nil {
		lookup = defaultLookupMX
	}
	res, err := lookup(ctx, domain)
	if err != nil {
		return true, err // fail open; DNS trouble must not lock everyone out
	}
	q.mxMu.Lock()
	q.mx[domain] = mxEntry{ok: res, expires: time.Now().Add(q.MXCacheTTL)}
	if len(q.mx) > 10000 {
		q.mx = map[string]mxEntry{}
	}
	q.mxMu.Unlock()
	return res, nil
}

// defaultLookupMX accepts a domain with MX records, or (RFC 5321 fallback)
// with an address record. "No such host" is a definitive no; other
// resolver errors are returned so the caller can fail open.
func defaultLookupMX(ctx context.Context, domain string) (bool, error) {
	mx, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err == nil && len(mx) > 0 {
		// A single null MX ("." with preference 0) means "no mail" (RFC 7505).
		if len(mx) == 1 && (mx[0].Host == "." || mx[0].Host == "") {
			return false, nil
		}
		return true, nil
	}
	var dnsErr *net.DNSError
	if err != nil && errors.As(err, &dnsErr) && !dnsErr.IsNotFound {
		return false, err
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err == nil {
		return len(addrs) > 0, nil
	}
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false, nil
	}
	return false, err
}

// redactAddress removes the address from an error message before logging.
func redactAddress(err error, email string) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), email, "[address]")
}

// --- single-use form tokens -----------------------------------------------------

// UsedTokens remembers consumed sign-in form tokens until they would have
// expired anyway, so one solved proof of work cannot be replayed.
type UsedTokens struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	ttl       time.Duration
	nextPrune int // size at which expired entries are swept next
}

// maxUsedTokens bounds memory under a flood of solved puzzles. Beyond it new
// tokens are refused until old ones expire; the global send budget would
// drop those sign-ins anyway.
const maxUsedTokens = 200000

// NewUsedTokens creates the set; ttl should equal the form token max age.
func NewUsedTokens(ttl time.Duration) *UsedTokens {
	return &UsedTokens{seen: map[string]time.Time{}, ttl: ttl, nextPrune: 1024}
}

// Consume marks tok used and reports whether it was fresh.
func (u *UsedTokens) Consume(tok string) bool {
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	if exp, ok := u.seen[tok]; ok && now.Before(exp) {
		return false
	}
	if len(u.seen) >= u.nextPrune {
		// Sweep only once the set has doubled since the last sweep, so the
		// cost stays amortized O(1) per token even when nothing has expired.
		for k, t := range u.seen {
			if now.After(t) {
				delete(u.seen, k)
			}
		}
		u.nextPrune = max(2*len(u.seen), 1024)
	}
	if len(u.seen) >= maxUsedTokens {
		return false
	}
	u.seen[tok] = now.Add(u.ttl)
	return true
}
