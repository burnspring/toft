package teamsbot

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"

	"github.com/google/uuid"
)

// manifestNamespace scopes the manifest ids this package derives. Fixed: a
// changed namespace is a new app in every organization's catalog.
var manifestNamespace = uuid.MustParse("6f1d7a52-3c0e-4f6b-9a77-2b8c5e0d4a11")

// ManifestID is a connection's Teams app id: the same on every download of one
// connection and different across connections. A fresh id per download
// would make each upload a second app in the org catalog, silently.
func ManifestID(connectionID string) string {
	return uuid.NewSHA1(manifestNamespace, []byte(connectionID)).String()
}

// PackageInfo is what the generated app says about itself.
type PackageInfo struct {
	ConnectionID string
	BotID        string // the bot's Entra application id — the connection's scope
	Name         string // the connection's name, which is the app's short name
	WebsiteURL   string // this install's public address
}

// Package builds the Teams app package an administrator uploads in the Teams
// admin center: manifest.json and the two icons. Scopes are `team` and
// `groupChat` — never `personal` — and the bot is notification-only, because
// Toft posts and never listens.
func Package(info PackageInfo) ([]byte, error) {
	short := info.Name
	if short == "" {
		short = "Toft"
	}
	if len([]rune(short)) > 30 {
		short = string([]rune(short)[:30])
	}
	site := info.WebsiteURL
	if site == "" {
		site = "https://toft.invalid"
	}
	manifest := map[string]any{
		"$schema":         "https://developer.microsoft.com/json-schemas/teams/v1.17/MicrosoftTeams.schema.json",
		"manifestVersion": "1.17",
		"version":         "1.0.0",
		"id":              ManifestID(info.ConnectionID),
		"developer": map[string]string{
			"name": "Toft", "websiteUrl": site, "privacyUrl": site, "termsOfUseUrl": site,
		},
		"name": map[string]string{"short": short, "full": short + " (Toft)"},
		"description": map[string]string{
			"short": "Messages from apps on Toft.",
			"full": "Posts messages from apps deployed on this organization's Toft install into " +
				"the teams and group chats it is added to. It does not read messages.",
		},
		"icons":       map[string]string{"color": "color.png", "outline": "outline.png"},
		"accentColor": "#2855C4",
		"bots": []map[string]any{{
			"botId":              info.BotID,
			"scopes":             []string{"team", "groupChat"},
			"isNotificationOnly": true,
			"supportsFiles":      false,
		}},
		"permissions":  []string{"identity"},
		"validDomains": []string{},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data func() ([]byte, error)
	}{
		{"manifest.json", func() ([]byte, error) { return raw, nil }},
		{"color.png", func() ([]byte, error) { return icon(192, true) }},
		{"outline.png", func() ([]byte, error) { return icon(32, false) }},
	} {
		data, err := f.data()
		if err != nil {
			return nil, err
		}
		w, err := z.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// icon draws Toft's mark — the roof over a block, web/public/favicon.svg's
// geometry — at size. The colour icon is the mark in white on the brand blue,
// full bleed; the outline icon is the mark in white on transparent, which is
// what Teams requires of it.
func icon(size int, filled bool) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	brand := color.NRGBA{0x28, 0x55, 0xc4, 0xff}
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	scale := float64(size) / 32
	// The colour icon insets the mark so it sits inside Teams' rounded tile.
	inset, shrink := 0.0, 1.0
	if filled {
		inset, shrink = 6*scale, 0.625
	}
	at := func(x, y float64) (float64, float64) {
		return inset + x*scale*shrink, inset + y*scale*shrink
	}
	ax, ay := at(4.5, 14.5)
	bx, by := at(16, 5)
	cx, cy := at(27.5, 14.5)
	half := 1.8 * scale * shrink
	rx0, ry0 := at(8, 17.5)
	rx1, ry1 := at(24, 27.5)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			mark := segDist(px, py, ax, ay, bx, by) <= half ||
				segDist(px, py, bx, by, cx, cy) <= half ||
				(px >= rx0 && px <= rx1 && py >= ry0 && py <= ry1)
			switch {
			case mark:
				img.SetNRGBA(x, y, white)
			case filled:
				img.SetNRGBA(x, y, brand)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}
