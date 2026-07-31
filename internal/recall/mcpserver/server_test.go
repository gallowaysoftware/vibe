package mcpserver

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/recall/store"
)

func TestDigestHandler(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.EnsureRepo(); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if err := st.CreateExpert("x4", "Terran playthrough"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	h := DigestHandler(Deps{Store: st, Version: "test"})

	cases := []struct {
		name       string
		url        string
		wantStatus int
		wantBody   string
	}{
		{"missing expert param", "/digest", 400, ""},
		{"unknown expert", "/digest?expert=nope", 404, ""},
		{"known expert", "/digest?expert=x4", 200, "# Memory digest: x4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", tc.url, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body missing %q:\n%s", tc.wantBody, rec.Body.String())
			}
		})
	}
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
