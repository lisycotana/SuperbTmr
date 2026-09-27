package locator

import (
	"strings"
	"testing"
)

func TestParseResourceURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantErr string // substring
		kind    Kind
		entry   string
		sid     string
		index   int
	}{
		// Entry locators
		{"entry bare", "mac", true, "", KindEntry, "mac", "", 0},
		{"entry scheme", "superbtmr://mac", true, "", KindEntry, "mac", "", 0},
		{"entry internal", "superbtmr://internal", true, "", KindEntry, "internal", "", 0},
		{"entry trailing slash", "superbtmr://mac/", true, "", KindEntry, "mac", "", 0},

		// Session locators (short form preferred)
		{"session short", "#abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session scheme short", "superbtmr://#abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session scheme short slash", "superbtmr://#/abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session session-prefix", "#session-abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session scheme session-prefix", "superbtmr://#session-abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session long form", "superbtmr://mac#abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session long form session-prefix", "superbtmr://mac#session-abc123def", true, "", KindSession, "", "abc123def", 0},

		// Shell locators
		{"shell short index", "#abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"shell short index 1", "#abc123def:1", true, "", KindShell, "", "abc123def", 1},
		{"shell scheme short index", "superbtmr://#abc123def:3", true, "", KindShell, "", "abc123def", 3},
		{"shell long form index", "superbtmr://mac#abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"shell scheme short slash index", "superbtmr://#/abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"session prefix with index", "#session-abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"long form session prefix with index", "superbtmr://mac#session-abc123def:2", true, "", KindShell, "", "abc123def", 2},

		// Errors
		{"empty", "", false, "empty", 0, "", "", 0},
		{"only hash", "#", false, "missing session id", 0, "", "", 0},
		{"only scheme", "superbtmr://", false, "malformed", 0, "", "", 0},
		{"notif uri", "superbtmr://shells/xyz", false, "notification broadcast URI", 0, "", "", 0},
		{"bad index zero", "#abc:0", false, "invalid shell index", 0, "", "", 0},
		{"bad index neg", "#abc:-1", false, "invalid shell index", 0, "", "", 0},
		{"bad index non-num", "#abc:x", false, "invalid shell index", 0, "", "", 0},
		{"empty index", "#abc123def:", false, "invalid shell index", 0, "", "", 0},
		{"long form bad index", "superbtmr://mac#abc:0", false, "invalid shell index", 0, "", "", 0},
		{"entry with colon no hash", "superbtmr://mac:2", false, "malformed", 0, "", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse(tc.raw)
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("expected error (contains %q), got nil: %+v", tc.wantErr, p)
				}
				if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want contains %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", p.Kind, tc.kind)
			}
			if p.Entry != tc.entry {
				t.Errorf("entry = %q, want %q", p.Entry, tc.entry)
			}
			if p.SessionID != tc.sid {
				t.Errorf("sid = %q, want %q", p.SessionID, tc.sid)
			}
			if p.Index != tc.index {
				t.Errorf("index = %d, want %d", p.Index, tc.index)
			}
		})
	}
}
