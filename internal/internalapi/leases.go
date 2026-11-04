package internalapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/utmapp/utm-games-db/internal/store"
)

// HolderHeader names the triage machine making a call. Triage writes
// (comments and status on a report) from any other machine than the one
// holding the report's lease are refused with 409.
const HolderHeader = "X-Triage-Holder"

func holderOf(r *http.Request) string { return strings.TrimSpace(r.Header.Get(HolderHeader)) }

// failLease answers 409 with the lease another machine holds.
func failLease(w http.ResponseWriter, err error) bool {
	var held *store.LeaseHeldError
	if !errors.As(err, &held) {
		return false
	}
	okStatus(w, http.StatusConflict, map[string]any{"error": held.Error(), "lease": held.Lease})
	return true
}

// leaseAcquire takes or renews a report's triage lease:
// {"holder": "...", "ttl_seconds": 1200} (holder defaults to the
// X-Triage-Holder header). 200 with the lease, or 409 with the lease another
// machine holds.
func (s *Server) leaseAcquire(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Holder string `json:"holder"`
		TTL    int    `json:"ttl_seconds"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if in.Holder == "" {
		in.Holder = holderOf(r)
	}
	if in.TTL == 0 {
		in.TTL = 1200
	}
	l, err := s.Store.AcquireTriageLease(r.Context(), id, in.Holder, time.Duration(in.TTL)*time.Second)
	if err != nil {
		if failLease(w, err) {
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			failFor(w, err)
		} else {
			fail(w, http.StatusBadRequest, err)
		}
		return
	}
	ok(w, map[string]any{"lease": l})
}

// leaseRelease ends the caller's lease (holder in ?holder= or the header).
func (s *Server) leaseRelease(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	holder := strings.TrimSpace(r.URL.Query().Get("holder"))
	if holder == "" {
		holder = holderOf(r)
	}
	if holder == "" {
		fail(w, http.StatusBadRequest, errors.New("holder: ?holder= or the "+HolderHeader+" header"))
		return
	}
	if err := s.Store.ReleaseTriageLease(r.Context(), id, holder); err != nil {
		if !failLease(w, err) {
			failFor(w, err)
		}
		return
	}
	ok(w, map[string]any{"report": id, "released": true})
}

// leaseGet is the report's lease row (404 when it never had one).
func (s *Server) leaseGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	l, err := s.Store.GetTriageLease(r.Context(), id)
	if err != nil {
		failFor(w, err)
		return
	}
	ok(w, map[string]any{"lease": l})
}
