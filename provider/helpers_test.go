package provider

import "testing"

func TestParseLocalID(t *testing.T) {
	cases := []struct {
		in        string
		kind, raw string
		ok        bool
	}{
		{"movie:abc", "movie", "abc", true},
		{"scene:a:b", "scene", "a:b", true},
		{"noprefix", "", "", false},
		{":abc", "", "", false},
		{"movie:", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		kind, raw, ok := ParseLocalID(c.in)
		if kind != c.kind || raw != c.raw || ok != c.ok {
			t.Errorf("ParseLocalID(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, kind, raw, ok, c.kind, c.raw, c.ok)
		}
	}
}

func TestYearFromDate(t *testing.T) {
	cases := map[string]int{
		"2021-05-09": 2021,
		"1998":       1998,
		"abc-01":     0,
		"20x1":       0,
		"":           0,
		"99":         0,
	}
	for in, want := range cases {
		if got := YearFromDate(in); got != want {
			t.Errorf("YearFromDate(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestSanitizeImageURL(t *testing.T) {
	cases := []struct {
		raw       string
		httpsOnly bool
		want      string
	}{
		{"https://cdn.example.test/a.jpg", true, "https://cdn.example.test/a.jpg"},
		{"http://cdn.example.test/a.jpg", true, ""}, // http rejected when httpsOnly
		{"http://stash.local:9999/img/1", false, "http://stash.local:9999/img/1"},
		{"https://stash.local/img/1", false, "https://stash.local/img/1"},
		{"file:///etc/passwd", false, ""}, // non-http scheme
		{"file:///etc/passwd", true, ""},
		{"javascript:alert(1)", false, ""}, // no host, not http
		{"/relative/path", true, ""},       // not absolute
		{"ftp://host/x", false, ""},        // disallowed scheme
		{"", true, ""},
		{"https://", true, ""}, // no host
	}
	for _, c := range cases {
		if got := SanitizeImageURL(c.raw, c.httpsOnly); got != c.want {
			t.Errorf("SanitizeImageURL(%q, %v) = %q, want %q", c.raw, c.httpsOnly, got, c.want)
		}
	}
}

func TestNamesToGenres(t *testing.T) {
	type tag struct{ Name string }
	in := []tag{{"action"}, {""}, {"drama"}}
	got := NamesToGenres(in, func(t tag) string { return t.Name })
	if len(got) != 2 || got[0] != "action" || got[1] != "drama" {
		t.Errorf("NamesToGenres = %#v, want [action drama]", got)
	}
}
