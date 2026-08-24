package dockerfile

import "testing"

func TestPrintApkRewrite(t *testing.T) {
	r, err := Rewrite([]byte("FROM alpine:3.20\nRUN apk add --no-cache curl\n"), Options{
		Project: "global", Base: "http://127.0.0.1:41999",
		Registry: "127.0.0.1:41999", AptProxy: "http://127.0.0.1:41999",
		Mode: Bridge,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", r.Content)
}
