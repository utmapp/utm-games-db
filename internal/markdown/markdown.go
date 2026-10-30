// Package markdown renders user supplied Markdown with links, images and raw
// HTML disabled; a trusted variant for the Auto-Triage bot keeps links. The
// user renderer also serves the live preview.
package markdown

import (
	"bytes"
	"html/template"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

var (
	md     goldmark.Markdown
	policy *bluemonday.Policy

	// Trusted variants for the Auto-Triage bot: links allowed, still no
	// images or raw HTML.
	mdTrusted     goldmark.Markdown
	policyTrusted *bluemonday.Policy
)

func init() {
	// Default inline parsers minus Link (which also handles images), AutoLink
	// and RawHTML. Code spans and emphasis remain.
	inline := []util.PrioritizedValue{
		util.Prioritized(parser.NewCodeSpanParser(), 100),
		util.Prioritized(parser.NewEmphasisParser(), 500),
	}
	p := parser.NewParser(
		parser.WithBlockParsers(parser.DefaultBlockParsers()...),
		parser.WithInlineParsers(inline...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)
	md = goldmark.New(
		goldmark.WithParser(p),
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
	// Defense in depth: even if a parser slips something through, strip it.
	policy = basePolicy()

	// Bot comments: default inline parsers (links and autolinks) minus RawHTML.
	trustedInline := []util.PrioritizedValue{
		util.Prioritized(parser.NewCodeSpanParser(), 100),
		util.Prioritized(parser.NewLinkParser(), 200),
		util.Prioritized(parser.NewAutoLinkParser(), 300),
		util.Prioritized(parser.NewEmphasisParser(), 500),
	}
	tp := parser.NewParser(
		parser.WithBlockParsers(parser.DefaultBlockParsers()...),
		parser.WithInlineParsers(trustedInline...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	)
	mdTrusted = goldmark.New(
		goldmark.WithParser(tp),
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList, extension.Linkify),
		goldmark.WithRendererOptions(html.WithHardWraps()),
	)
	policyTrusted = basePolicy()
	policyTrusted.AllowAttrs("href").OnElements("a")
	policyTrusted.AllowURLSchemes("http", "https", "mailto")
	policyTrusted.AddTargetBlankToFullyQualifiedLinks(true) // also sets rel="noopener noreferrer"
}

func basePolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "strong", "em", "b", "i", "del", "s", "code", "pre",
		"blockquote", "ul", "ol", "li", "h1", "h2", "h3", "h4", "h5", "h6", "hr",
		"table", "thead", "tbody", "tr", "th", "td")
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("code", "pre", "li", "ul")
	p.AllowAttrs("align").Matching(bluemonday.CellAlign).OnElements("th", "td")
	p.AllowAttrs("type", "checked", "disabled").OnElements("input")
	p.AllowElements("input")
	p.AllowAttrs("start").Matching(bluemonday.Integer).OnElements("ol")
	return p
}

// Render converts user Markdown to sanitized HTML with links disabled.
func Render(src string) template.HTML {
	return render(md, policy, src)
}

// RenderTrusted converts Markdown written by the Auto-Triage bot; links are
// kept (opening in a new tab), images and raw HTML are still stripped.
func RenderTrusted(src string) template.HTML {
	return render(mdTrusted, policyTrusted, src)
}

func render(m goldmark.Markdown, p *bluemonday.Policy, src string) template.HTML {
	var buf bytes.Buffer
	if err := m.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(p.SanitizeBytes(buf.Bytes()))
}
