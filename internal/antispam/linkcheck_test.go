package antispam

import "testing"

func TestFindLinks(t *testing.T) {
	bad := []string{
		"see https://example.com/x",
		"HTTP://EXAMPLE.COM",
		"go to www.example.com now",
		"[here](http://x)",
		"[here](/relative)",
		"<https://x.y>",
		"<b>bold</b>",
		"<img src=x>",
		"mailto:a@b.c",
		"ftp://files",
	}
	for _, s := range bad {
		if got := FindLinks(s, 5); len(got) == 0 {
			t.Errorf("FindLinks(%q) should flag", s)
		}
	}
	good := []string{
		"The game crashes at startup.",
		"config.plist and game.exe are fine",
		"Use 4 < 5 and 6 > 3 in math",
		"array[0](1) is not a link? [x] (y)",
		"IP 10.0.0.1 and version 5.0.8",
		"",
	}
	for _, s := range good {
		if got := FindLinks(s, 5); len(got) != 0 {
			t.Errorf("FindLinks(%q) = %v; should be clean", s, got)
		}
	}
}
