package store

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

// lossyTransport delivers requests matching lose to next (so the fake portal
// commits them) and then reports a lost response, like a reset after commit.
type lossyTransport struct {
	next http.RoundTripper
	lose func(*http.Request) bool
}

func (l lossyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := l.next.RoundTrip(r)
	if err != nil || !l.lose(r) {
		return res, err
	}
	_ = res.Body.Close()
	return nil, errors.New("connection reset by peer")
}

func loseResponses(k *tooltest.Kit, lose func(*http.Request) bool) {
	k.Env.HTTP = &http.Client{Transport: tool.GuardWrites(lossyTransport{next: k.HTTP, lose: lose})}
}

func isPost(suffix string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, suffix)
	}
}

// SA-1: an in-root symlink is never uploaded, whatever its name.
func TestUploadRefusesSymlink(t *testing.T) {
	k, f := setup(t)
	root := k.Files.Root
	if err := os.WriteFile(filepath.Join(root, "secret.der"), []byte("\x89PNG\r\n\x1a\nkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secret.der", filepath.Join(root, "shot.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "s.png"), []byte("\x89PNG\r\n\x1a\nimg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "assets")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"shot.png", "assets/s.png", filepath.Join(root, "assets", "s.png")} {
		res := tooltest.Call(t, lookup(t, "store_screenshots"), k.Env, `{"app":"Bike Guide","action":"add","path":"`+filepath.ToSlash(p)+`"}`)
		if body := tooltest.MustFail(t, res, "invalid_arguments").Message; !strings.Contains(body, "symlink") {
			t.Fatalf("%s: %s", p, body)
		}
	}
	if len(k.HTTP.Mutations()) != 0 || f.called("/screenshots") {
		t.Fatalf("uploaded: %v", f.calls)
	}
	// The real file is accepted and the approval shows its root-relative path.
	res := tooltest.MustOK(t, tooltest.Call(t, lookup(t, "store_screenshots"), k.Env, `{"app":"Bike Guide","action":"add","path":"real/s.png"}`))
	if !strings.Contains(tooltest.JSON(t, res.Data), `"file":"real/s.png"`) {
		t.Fatalf("preview lacks the relative path: %s", tooltest.JSON(t, res.Data))
	}
}

// SA-1: content must match the extension's signature.
func TestUploadRefusesWrongMagic(t *testing.T) {
	k, f := setup(t)
	if err := k.Files.WriteFile("shot.png", []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := tooltest.Call(t, lookup(t, "store_screenshots"), k.Env, `{"app":"Bike Guide","action":"add","path":"shot.png"}`)
	if body := tooltest.MustFail(t, res, "invalid_arguments").Message; !strings.Contains(body, "not a png") {
		t.Fatalf("png: %s", body)
	}
	if err := k.Files.WriteFile("app.iq", []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = tooltest.Call(t, lookup(t, "store_version_publish"), k.Env, `{"app":"Bike Guide","path":"app.iq","version":"1.1.0","other_locales":"keep"}`)
	tooltest.MustFail(t, res, "invalid_arguments")
	if f.called("/iqFiles/validate") {
		t.Fatal("a non-ZIP .iq was uploaded")
	}
}

// LF-3: a version POST committed by the portal whose response was lost is
// reported as published after a metadata re-read, and what's new is applied.
func TestVersionPublishLostResponseRecovered(t *testing.T) {
	k, f := setup(t)
	writeIQ(t, k, "IQ")
	args, _ := publishPlan(t, k, `{"app":"Bike Guide","path":"app.iq","version":"1.1.0","whats_new":{"en":"new"},"other_locales":"keep"}`)
	loseResponses(k, isPost("/version"))
	res := tooltest.MustOK(t, tooltest.Call(t, lookup(t, "store_version_publish"), k.Env, args))
	body := tooltest.JSON(t, res.Data)
	if !strings.Contains(body, `"status":"published"`) || f.putBody == nil {
		t.Fatalf("lost-but-committed publish: %s (put %v)", body, f.putBody != nil)
	}
}

// LF-3: a lost version POST the portal does not show is outcome_unknown,
// never a plain transport error.
func TestVersionPublishLostResponseUnknown(t *testing.T) {
	k, _ := setup(t)
	writeIQ(t, k, "IQ")
	args, _ := publishPlan(t, k, `{"app":"Bike Guide","path":"app.iq","version":"1.1.0","other_locales":"keep"}`)
	k.Env.HTTP = &http.Client{Transport: tool.GuardWrites(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isPost("/version")(r) {
			return nil, errors.New("timeout")
		}
		return k.HTTP.RoundTrip(r)
	}))}
	res := tooltest.Call(t, lookup(t, "store_version_publish"), k.Env, args)
	if body := tooltest.MustFail(t, res, "outcome_unknown").Message; !strings.Contains(body, "Do NOT bump") {
		t.Fatalf("unknown publish: %s", body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeCreateFiles(t *testing.T, k *tooltest.Kit) string {
	t.Helper()
	writeIQ(t, k, "IQ")
	for _, n := range []string{"icon.png", "s1.png"} {
		if err := k.Files.WriteFile(n, []byte("\x89PNG\r\n\x1a\nimg"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return `{"path":"app.iq","icon":"icon.png","screenshots":["s1.png"],"version":"1.0.0","category":"168","dev_email":"a@b.c","locales":[{"locale":"en","title":"New","description":"D"}],"confirm":true}`
}

// LF-6: an app whose manifest id already exists is never submitted again.
func TestAppCreateRefusesDuplicateManifest(t *testing.T) {
	k, f := setup(t)
	args := writeCreateFiles(t, k)
	res := tooltest.Call(t, lookup(t, "store_app_create"), k.Env, args)
	if body := tooltest.MustFail(t, res, "invalid_arguments").Message; !strings.Contains(body, "app already exists: "+appID) {
		t.Fatalf("duplicate: %s", body)
	}
	if f.count("POST "+devBase+"/apps") != 0 {
		t.Fatal("duplicate app submitted")
	}
	// A new manifest goes through.
	f.validManifest = "cccccccccccccccccccccccccccccccc"
	tooltest.MustOK(t, tooltest.Call(t, lookup(t, "store_app_create"), k.Env, args))
	if f.count("POST "+devBase+"/apps") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
}

// LF-6: a lost CreateApp response is outcome_unknown pointing at
// store_apps_list.
func TestAppCreateLostResponseUnknown(t *testing.T) {
	k, f := setup(t)
	f.validManifest = "cccccccccccccccccccccccccccccccc"
	args := writeCreateFiles(t, k)
	loseResponses(k, isPost(devBase+"/apps"))
	res := tooltest.Call(t, lookup(t, "store_app_create"), k.Env, args)
	if body := tooltest.MustFail(t, res, "outcome_unknown").Message; !strings.Contains(body, "store_apps_list") {
		t.Fatalf("unknown create: %s", body)
	}
}

// LF-4: a 403 on an app endpoint is a forbidden api_error, not auth_required.
func TestForbiddenIsNotAuthRequired(t *testing.T) {
	k, f := setup(t)
	k.HTTP.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metadata") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.ServeHTTP(w, r)
	})
	res := tooltest.Call(t, lookup(t, "store_app_get"), k.Env, `{"app":"Bike Guide"}`)
	if b := tooltest.MustFail(t, res, "api_error"); !strings.Contains(b.Hint, "do not call auth_login") {
		t.Fatalf("hint: %+v", b)
	}
}
