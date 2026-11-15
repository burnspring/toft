package toft

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func issueToken(t *testing.T, minutes int, calls *int) {
	t.Helper()
	resetTokenCache()
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/app/tokens/sharepoint" {
			writeJSON(t, w, 404, map[string]string{"error": "no such route"})
			return
		}
		*calls++
		writeJSON(t, w, 200, map[string]any{
			"access_token": "eyJ.tok", "token_type": "Bearer",
			"expires_at": time.Now().Add(time.Duration(minutes) * time.Minute).UTC().Format(time.RFC3339),
			"resource":   "https://graph.microsoft.com",
			"folder":     map[string]string{"drive_id": "b!d", "item_id": "01R", "label": "reports"},
		})
	})
}

func TestATokenIsFetchedOnceAndCached(t *testing.T) {
	calls := 0
	issueToken(t, 60, &calls)
	ctx := context.Background()
	first, err := AccessToken(ctx, "sharepoint")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := AccessToken(ctx, "sharepoint")
	if first.Value != "eyJ.tok" || again.Value != first.Value || calls != 1 {
		t.Errorf("first %+v, again %+v, %d calls", first, again, calls)
	}
	if first.Folder.DriveID != "b!d" || first.Folder.Label != "reports" {
		t.Errorf("folder = %+v", first.Folder)
	}
	if strings.Contains(first.String(), "eyJ") {
		t.Error("String prints the token")
	}
}

func TestATokenNearExpiryIsReplaced(t *testing.T) {
	calls := 0
	issueToken(t, 4, &calls)
	AccessToken(context.Background(), "sharepoint")
	AccessToken(context.Background(), "sharepoint")
	if calls != 2 {
		t.Errorf("%d calls, want 2", calls)
	}
}

func TestATokenRefusalCarriesThePlatformsWords(t *testing.T) {
	resetTokenCache()
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 403, map[string]string{
			"error": "this SharePoint connection does not hand out access tokens",
			"code":  "sharepoint_tokens_off"})
	})
	_, err := AccessToken(context.Background(), "sharepoint")
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(err.Error(), "does not hand out access tokens") {
		t.Errorf("err = %v", err)
	}
}
