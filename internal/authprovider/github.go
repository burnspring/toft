package authprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// GitHub, the one non-OIDC kind.
//
// It predates OIDC: there is no discovery document, no id_token, and the
// profile lives behind a bespoke API whose primary email needs a second
// request. Kept as its own kind rather than bent into the OIDC path, because
// every part of that path — issuer, `sub`, userinfo — has no counterpart here.

var githubEndpoints = Endpoints{
	AuthURL:     "https://github.com/login/oauth/authorize",
	TokenURL:    "https://github.com/login/oauth/access_token",
	UserinfoURL: "https://api.github.com/user",
}

func githubProfile(ctx context.Context, client *http.Client) (*Profile, error) {
	var user struct {
		ID        int    `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := githubGet(ctx, client, githubEndpoints.UserinfoURL, &user); err != nil {
		return nil, err
	}
	if user.ID == 0 {
		return nil, fmt.Errorf("github returned no user id")
	}

	profile := &Profile{
		// The numeric id, not the login: a GitHub handle can be renamed and
		// then claimed by someone else, so it is a display name and not an
		// identity.
		Subject:   strconv.Itoa(user.ID),
		Login:     user.Login,
		Name:      user.Name,
		Email:     user.Email,
		AvatarURL: user.AvatarURL,
	}

	// A private primary address is absent from /user, so it takes a second
	// request. That request is also the only place GitHub says whether the
	// address is *verified*, which is what account linking turns on — so it
	// runs even when /user did return an address.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := githubGet(ctx, client, "https://api.github.com/user/emails", &emails); err == nil {
		for _, e := range emails {
			if e.Primary {
				profile.Email = e.Email
				profile.EmailVerified = e.Verified
				profile.EmailUnverified = !e.Verified
				break
			}
		}
	}
	return profile, nil
}

func githubGet(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
