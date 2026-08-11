package api

import (
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/store"
)

// The line handed over has to name the variable the address was really read
// from. Deriving one looks like it works and is wrong for every project that
// writes MS_AUTH_ADDR or MS_AUTH_HOST — both of which discovery accepts — and
// serving the guess beside the stored name gives an agent two lines and no way
// to tell which one anything reads.
func TestPointAtNamesTheVariableThatWasRead(t *testing.T) {
	cases := []struct {
		name string
		svc  store.Service
		want string
	}{
		{
			"the stored variable wins over anything derivable",
			store.Service{Name: "ms-auth", Protocol: "grpc", Listen: "127.0.0.1:9152", EnvKey: "MS_AUTH_ADDR"},
			"MS_AUTH_ADDR=127.0.0.1:9152",
		},
		{
			"and it still carries the scheme a TLS listener needs",
			store.Service{Name: "ms-auth", Protocol: "http", Listen: "127.0.0.1:9152", EnvKey: "MS_AUTH_HOST", TLS: true},
			"MS_AUTH_HOST=https://127.0.0.1:9152",
		},
		{
			"nothing was read, so the name is derived from the service",
			store.Service{Name: "ms-auth", Protocol: "grpc", Listen: "127.0.0.1:9152"},
			"MS_AUTH_GRPC_URL=127.0.0.1:9152",
		},
		{
			"and http keeps its own suffix",
			store.Service{Name: "ms-rates", Protocol: "http", Listen: "127.0.0.1:9153"},
			"MS_RATES_URL=127.0.0.1:9153",
		},
	}

	for _, c := range cases {
		if got := pointAt(c.svc); got != c.want {
			t.Errorf("%s: pointAt = %q, want %q", c.name, got, c.want)
		}
	}
}
