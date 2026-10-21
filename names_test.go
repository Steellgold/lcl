package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	cases := []struct{ input, want string }{
		{"Etat de première version", "etat-de-premiere-version"},
		{"ÉTAT DE PREMIÈRE VERSION", "etat-de-premiere-version"},
		{"E\u0301tat de premie\u0300re version", "etat-de-premiere-version"},
		{"  --My__First...Release--  ", "my-first-release"},
		{"Œuvre & Cœur", "oeuvre-coeur"},
		{"Straße", "strasse"},
		{"v1.2 / feature", "v1-2-feature"},
		{"release 🚀 candidate", "release-candidate"},
		{"123", "123"},
		{strings.Repeat("é", 63), strings.Repeat("e", 63)},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := normalizeName(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("normalizeName(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
			}
		})
	}
	for _, input := range []string{"", "   ", "---", "🚀", "\u0301", strings.Repeat("a", 64)} {
		t.Run("invalid "+input, func(t *testing.T) {
			if _, err := normalizeName(input); err == nil {
				t.Fatal("invalid name accepted")
			}
		})
	}
}

func TestClientNameArguments(t *testing.T) {
	for _, args := range [][]string{
		{"3000", "--name", "Etat de première version"},
		{"--name", "Etat de première version", "3000"},
		{"3000", "--name=Etat de première version", "-t", "10", "--https"},
	} {
		r, action, err := parseClientArgs(args)
		if err != nil || action != "" || r.Name != "etat-de-premiere-version" {
			t.Fatalf("%v: %+v, %q, %v", args, r, action, err)
		}
	}
	for _, args := range [][]string{
		{"3000", "--name"}, {"3000", "--name="},
		{"3000", "--name", "---"}, {"3000", "--name", strings.Repeat("a", 64)},
	} {
		if _, _, err := parseClientArgs(args); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
}

func TestNamedSessionCollisionAndReuse(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()
	b := testBroker(t)
	r := request{Port: backendPort(t, origin), Name: "Etat de première version"}
	first, err := b.add(1, r)
	if err != nil {
		t.Fatal(err)
	}
	defer b.remove(first)
	if first.host != "etat-de-premiere-version."+b.domain {
		t.Fatal(first.host)
	}
	for _, uid := range []uint32{1, 2} {
		collision := r
		collision.Name = "etat-de-premiere-version"
		if _, err := b.add(uid, collision); err == nil || !strings.Contains(err.Error(), "already in use") {
			t.Fatalf("uid %d: %v", uid, err)
		}
	}
	b.remove(first)
	second, err := b.add(2, r)
	if err != nil {
		t.Fatal(err)
	}
	defer b.remove(second)
	// A delayed cleanup must never remove a later session with the same name.
	b.remove(first)
	if b.sessions[second.host] != second {
		t.Fatal("cleanup removed the replacement session")
	}
	data, err := os.ReadFile(b.config)
	if err != nil || !strings.Contains(string(data), second.host) {
		t.Fatalf("replacement route missing: %s, %v", data, err)
	}
}

func TestNamedRoutePriority(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()
	for _, name := range []string{"", "my-preview"} {
		b := testBroker(t)
		s, err := b.add(1, request{Port: backendPort(t, origin), Name: name, HTTPS: true})
		if err != nil {
			t.Fatal(err)
		}
		defer b.remove(s)
		var config struct {
			HTTP struct {
				Routers map[string]struct {
					Priority int `json:"priority"`
				} `json:"routers"`
			} `json:"http"`
		}
		data, err := os.ReadFile(b.config)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		want := 100
		if name != "" {
			want = 1
		}
		if len(config.HTTP.Routers) != 2 {
			t.Fatal("expected HTTPS and redirect routes")
		}
		for router, value := range config.HTTP.Routers {
			if value.Priority != want {
				t.Fatalf("%s priority = %d; want %d", router, value.Priority, want)
			}
		}
	}
}

func TestBrokerRejectsInvalidNames(t *testing.T) {
	b := testBroker(t)
	for _, name := range []string{"---", strings.Repeat("a", 64)} {
		if _, err := b.add(1, request{Port: 3000, Name: name}); err == nil {
			t.Fatal("invalid name accepted")
		}
	}
	b.domain = strings.Repeat("a.", 120) + "example.test"
	if _, err := b.add(1, request{Port: 3000, Name: "long", Tunnel: true}); err == nil {
		t.Fatal("oversized hostname accepted")
	}
}
