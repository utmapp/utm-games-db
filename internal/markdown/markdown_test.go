package markdown

import (
	"strings"
	"testing"
)

func TestRenderBlocksLinksAndHTML(t *testing.T) {
	cases := []struct {
		in       string
		mustHave []string
		mustNot  []string
	}{
		{"**bold** and _em_ and `code`", []string{"<strong>bold</strong>", "<em>em</em>", "<code>code</code>"}, nil},
		{"[click](https://evil.example)", []string{"[click](https://evil.example)"}, []string{"<a"}},
		{"<https://evil.example>", nil, []string{"<a", "href"}},
		{"https://evil.example/path", []string{"https://evil.example/path"}, []string{"<a"}},
		{"![x](https://evil.example/i.png)", nil, []string{"<img"}},
		{"<script>alert(1)</script>", nil, []string{"<script"}},
		{"<b onclick=x>hi</b>", []string{"&lt;b onclick"}, []string{"<b"}},
		{"| a | b |\n|---|---|\n| 1 | 2 |", []string{"<table>", "<td>1</td>"}, nil},
		{"- [ ] todo\n- [x] done", []string{"<li", "checkbox"}, nil},
		{"```\nlog line <tag>\n```", []string{"<pre>", "&lt;tag&gt;"}, []string{"<tag>"}},
		{"~~gone~~", []string{"<del>gone</del>"}, nil},
	}
	for _, c := range cases {
		out := string(Render(c.in))
		for _, h := range c.mustHave {
			if !strings.Contains(out, h) {
				t.Errorf("Render(%q) = %q; missing %q", c.in, out, h)
			}
		}
		for _, n := range c.mustNot {
			if strings.Contains(out, n) {
				t.Errorf("Render(%q) = %q; must not contain %q", c.in, out, n)
			}
		}
	}
}

func TestRenderTrustedKeepsLinksOnly(t *testing.T) {
	out := string(RenderTrusted("See [the PR](https://github.com/utmapp/UTM/pull/1) or https://example.com/x ![i](https://e/i.png) <b onclick=x>hi</b> <script>1</script> [js](javascript:alert(1))"))
	for _, h := range []string{`href="https://github.com/utmapp/UTM/pull/1"`, `href="https://example.com/x"`, `rel="noopener`, `target="_blank"`, "the PR"} {
		if !strings.Contains(out, h) {
			t.Errorf("RenderTrusted missing %q in %q", h, out)
		}
	}
	for _, n := range []string{"<img", "<script", "<b", "javascript:"} {
		if strings.Contains(out, n) {
			t.Errorf("RenderTrusted must not contain %q in %q", n, out)
		}
	}
}
