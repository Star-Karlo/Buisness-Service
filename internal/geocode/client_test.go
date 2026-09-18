package geocode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchSendsTheServiceTokenAndDecodesCandidates(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"kind":"road","name":"Jl. Raya Bekasi","label":"Jl. Raya Bekasi, Cikarang Utara, Bekasi, Jawa Barat","address":{"kecamatan":"Cikarang Utara","kabupaten":"Bekasi","provinsi":"Jawa Barat"},"lat":-6.26,"lon":107.15}]`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "svc")
	out, err := c.Search(context.Background(), "raya bekasi", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/search" || got.URL.Query().Get("q") != "raya bekasi" || got.URL.Query().Get("limit") != "5" {
		t.Fatalf("request = %s", got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer svc" {
		t.Fatalf("Authorization = %q", got.Header.Get("Authorization"))
	}
	if len(out) != 1 || out[0].Label == "" || out[0].Lat != -6.26 || out[0].Address.Kabupaten != "Bekasi" {
		t.Fatalf("decoded %+v", out)
	}
}

func TestUnconfiguredClientRefuses(t *testing.T) {
	if _, err := New("", "").Search(context.Background(), "x", 1); err != ErrNotConfigured {
		t.Fatalf("err = %v", err)
	}
}
