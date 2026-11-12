package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func newProviderServer(t *testing.T, profile string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not used", http.StatusNotFound)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clientID, clientSecret := r.Form.Get("client_id"), r.Form.Get("client_secret")
		if clientID == "" {
			clientID, clientSecret, _ = r.BasicAuth()
		}
		if r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("code") != "test-code" ||
			clientID != "client-id" ||
			clientSecret != "client-secret" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(profile))
	})
	return httptest.NewServer(mux)
}

func TestProviders(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		build      func(ProviderConfig) *Provider
		wantUserID string
		wantLogin  string
		wantEmail  string
	}{
		{
			name:       "github",
			profile:    `{"id":42,"login":"octocat","email":"octocat@example.com"}`,
			build:      NewGitHub,
			wantUserID: "42",
			wantLogin:  "octocat",
			wantEmail:  "octocat@example.com",
		},
		{
			name:       "google",
			profile:    `{"sub":"google-1","email":"alice@example.com","email_verified":true}`,
			build:      NewGoogle,
			wantUserID: "google-1",
			wantLogin:  "alice",
			wantEmail:  "alice@example.com",
		},
		{
			name:       "yandex",
			profile:    `{"id":"yandex-1","login":"bob","default_email":"bob@example.com"}`,
			build:      NewYandex,
			wantUserID: "yandex-1",
			wantLogin:  "bob",
			wantEmail:  "bob@example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newProviderServer(t, tt.profile)
			defer server.Close()

			provider := tt.build(ProviderConfig{
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				RedirectURL:  "http://localhost/auth/" + tt.name + "/callback",
				AuthURL:      server.URL + "/authorize",
				TokenURL:     server.URL + "/oauth/token",
				UserInfoURL:  server.URL + "/userinfo",
			})

			authURL := provider.AuthCodeURL("state-value")
			for _, want := range []string{"client_id=client-id", "state=state-value", "redirect_uri="} {
				if !strings.Contains(authURL, want) {
					t.Errorf("AuthCodeURL() = %q, missing %q", authURL, want)
				}
			}

			token, err := provider.Exchange(context.Background(), "test-code")
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if token.AccessToken != "access-token" {
				t.Errorf("access token = %q, want access-token", token.AccessToken)
			}

			identity, err := provider.FetchIdentity(context.Background(), token)
			if err != nil {
				t.Fatalf("FetchIdentity() error = %v", err)
			}
			if identity.ProviderUserID != tt.wantUserID ||
				identity.Login != tt.wantLogin ||
				identity.Email != tt.wantEmail {
				t.Errorf("identity = %+v, want user %q login %q email %q",
					identity, tt.wantUserID, tt.wantLogin, tt.wantEmail)
			}
		})
	}
}

func TestFetchIdentityRejectsBadProfiles(t *testing.T) {
	tests := []struct {
		name    string
		build   func(ProviderConfig) *Provider
		profile string
	}{
		{name: "github missing id", build: NewGitHub, profile: `{"login":"octocat"}`},
		{name: "github missing login", build: NewGitHub, profile: `{"id":42}`},
		{name: "google missing email", build: NewGoogle, profile: `{"sub":"google-1"}`},
		{name: "yandex missing id", build: NewYandex, profile: `{"login":"bob"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newProviderServer(t, tt.profile)
			defer server.Close()
			provider := tt.build(ProviderConfig{
				ClientID:     "client-id",
				ClientSecret: "client-secret",
				AuthURL:      server.URL + "/authorize",
				TokenURL:     server.URL + "/oauth/token",
				UserInfoURL:  server.URL + "/userinfo",
			})
			token, err := provider.Exchange(context.Background(), "test-code")
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if _, err := provider.FetchIdentity(context.Background(), token); err == nil {
				t.Fatal("FetchIdentity() error = nil, want error")
			}
		})
	}
}

func TestFetchIdentityReportsHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := NewGitHub(ProviderConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		UserInfoURL:  server.URL,
	})
	token := &oauth2.Token{AccessToken: "access-token", TokenType: "Bearer"}
	if _, err := provider.FetchIdentity(context.Background(), token); err == nil {
		t.Fatal("FetchIdentity() error = nil, want error")
	}
}

func TestYandexTokenUsesFormAuth(t *testing.T) {
	var gotClientID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotClientID = r.Form.Get("client_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"bearer"}`))
	}))
	defer server.Close()

	provider := NewYandex(ProviderConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		TokenURL:     server.URL,
	})
	if _, err := provider.Exchange(context.Background(), "code"); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if gotClientID != "client-id" {
		t.Errorf("client_id = %q, want client-id", gotClientID)
	}
}

func TestProviderConfigOverridesDefaults(t *testing.T) {
	provider := NewGitHub(ProviderConfig{
		ClientID:     "id",
		ClientSecret: "secret",
		AuthURL:      "https://auth.example.com",
		TokenURL:     "https://token.example.com",
		UserInfoURL:  "https://user.example.com",
	})
	authURL := provider.AuthCodeURL("s")
	if !strings.HasPrefix(authURL, "https://auth.example.com?") {
		t.Errorf("AuthCodeURL() = %q, want auth.example.com", authURL)
	}
	if provider.userInfoURL != "https://user.example.com" {
		t.Errorf("userInfoURL = %q", provider.userInfoURL)
	}
	if provider.config.Endpoint.TokenURL != "https://token.example.com" {
		t.Errorf("token URL = %q", provider.config.Endpoint.TokenURL)
	}
}
