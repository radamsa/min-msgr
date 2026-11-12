// Package oauth implements the login flow for the supported OAuth providers.
//
// Providers are intentionally small: authorization URL generation, code
// exchange, and mapping a provider profile to a local identity. Adding a new
// provider means adding a constructor and registering it in the web handler.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

var yandexEndpoint = oauth2.Endpoint{
	AuthURL:   "https://oauth.yandex.ru/authorize",
	TokenURL:  "https://oauth.yandex.ru/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// Identity is the provider-independent profile used to log a user in.
type Identity struct {
	ProviderUserID string
	Login          string
	Email          string
}

// ProviderConfig contains credentials and optional endpoint overrides. Empty
// endpoint fields use the provider's production URLs; tests override them.
type ProviderConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
}

// Provider performs one OAuth provider's login flow.
type Provider struct {
	name        string
	config      *oauth2.Config
	userInfoURL string
	fetch       func(context.Context, *http.Client) (Identity, error)
}

func (p *Provider) Name() string {
	return p.name
}

// AuthCodeURL returns the provider URL the browser should be sent to.
func (p *Provider) AuthCodeURL(state string) string {
	return p.config.AuthCodeURL(state)
}

// Exchange swaps an authorization code for an access token.
func (p *Provider) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	token, err := p.config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange %s authorization code: %w", p.name, err)
	}
	return token, nil
}

// FetchIdentity loads the authenticated user's profile.
func (p *Provider) FetchIdentity(ctx context.Context, token *oauth2.Token) (Identity, error) {
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(token))
	client.Timeout = 10 * time.Second

	identity, err := p.fetch(ctx, client)
	if err != nil {
		return Identity{}, fmt.Errorf("fetch %s profile: %w", p.name, err)
	}
	identity.Login = strings.TrimSpace(identity.Login)
	identity.Email = strings.TrimSpace(identity.Email)
	identity.ProviderUserID = strings.TrimSpace(identity.ProviderUserID)
	if identity.ProviderUserID == "" {
		return Identity{}, fmt.Errorf("%s returned an empty user id", p.name)
	}
	if identity.Login == "" {
		return Identity{}, fmt.Errorf("%s returned an empty login", p.name)
	}
	return identity, nil
}

// NewGitHub builds the GitHub provider. GitHub is OAuth2, not OIDC, so the
// login comes from the REST profile.
func NewGitHub(cfg ProviderConfig) *Provider {
	endpoint := github.Endpoint
	userInfoURL := cfg.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = "https://api.github.com/user"
	}
	p := &Provider{
		name:        "github",
		userInfoURL: userInfoURL,
	}
	p.config = providerConfig(cfg, endpoint, []string{"read:user"})
	p.fetch = func(ctx context.Context, client *http.Client) (Identity, error) {
		var profile struct {
			ID    int64   `json:"id"`
			Login string  `json:"login"`
			Email *string `json:"email"`
		}
		if err := getJSON(ctx, client, p.userInfoURL, &profile); err != nil {
			return Identity{}, err
		}
		if profile.ID <= 0 {
			return Identity{}, fmt.Errorf("github profile has no id")
		}
		identity := Identity{
			ProviderUserID: strconv.FormatInt(profile.ID, 10),
			Login:          profile.Login,
		}
		if profile.Email != nil {
			identity.Email = *profile.Email
		}
		return identity, nil
	}
	return p
}

// NewGoogle builds the Google OpenID Connect provider.
func NewGoogle(cfg ProviderConfig) *Provider {
	userInfoURL := cfg.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	}
	p := &Provider{
		name:        "google",
		userInfoURL: userInfoURL,
	}
	p.config = providerConfig(cfg, google.Endpoint, []string{"openid", "email", "profile"})
	p.fetch = func(ctx context.Context, client *http.Client) (Identity, error) {
		var profile struct {
			Sub   string `json:"sub"`
			Email string `json:"email"`
		}
		if err := getJSON(ctx, client, p.userInfoURL, &profile); err != nil {
			return Identity{}, err
		}
		login := profile.Email
		if at := strings.Index(login, "@"); at > 0 {
			login = login[:at]
		}
		return Identity{
			ProviderUserID: profile.Sub,
			Login:          login,
			Email:          profile.Email,
		}, nil
	}
	return p
}

// NewYandex builds the Yandex OAuth provider.
func NewYandex(cfg ProviderConfig) *Provider {
	userInfoURL := cfg.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = "https://login.yandex.ru/info?format=json"
	}
	p := &Provider{
		name:        "yandex",
		userInfoURL: userInfoURL,
	}
	p.config = providerConfig(cfg, yandexEndpoint, []string{"login:email", "login:info"})
	p.fetch = func(ctx context.Context, client *http.Client) (Identity, error) {
		var profile struct {
			ID           json.RawMessage `json:"id"`
			Login        string          `json:"login"`
			DefaultEmail string          `json:"default_email"`
			Emails       []string        `json:"emails"`
		}
		if err := getJSON(ctx, client, p.userInfoURL, &profile); err != nil {
			return Identity{}, err
		}
		identity := Identity{
			ProviderUserID: strings.Trim(string(profile.ID), `"`),
			Login:          profile.Login,
			Email:          profile.DefaultEmail,
		}
		if identity.Email == "" && len(profile.Emails) > 0 {
			identity.Email = profile.Emails[0]
		}
		return identity, nil
	}
	return p
}

func providerConfig(cfg ProviderConfig, endpoint oauth2.Endpoint, scopes []string) *oauth2.Config {
	if cfg.AuthURL != "" {
		endpoint.AuthURL = cfg.AuthURL
	}
	if cfg.TokenURL != "" {
		endpoint.TokenURL = cfg.TokenURL
	}
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Scopes:       scopes,
		Endpoint:     endpoint,
	}
}

func getJSON(ctx context.Context, client *http.Client, url string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request profile: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("profile request returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode profile: %w", err)
	}
	return nil
}
