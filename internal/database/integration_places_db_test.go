package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A place is one row per (connection, conversation); what points into it is
// found by the destination prefix the transport supplies; and a connection's
// deletion takes its places with it.
func TestPlacesAreUniquePerConnectionAndKnowWhoPointsIntoThem(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "bot-poster")
	conn := connect(t, db, "iconn-bot", "teamsbot", "Bot")

	team := &database.IntegrationPlace{ID: "pl-1", ConnectionID: conn.ID, Kind: database.PlaceTeam,
		ConversationID: "19:team@thread.tacv2", Name: "Toft Test"}
	if err := db.AddPlace(team, "", database.RequestMeta{}); err != nil {
		t.Fatalf("AddPlace: %v", err)
	}
	dup := *team
	dup.ID = "pl-2"
	if err := db.AddPlace(&dup, "", database.RequestMeta{}); err == nil {
		t.Error("the same conversation was added to one connection twice")
	}
	chat := &database.IntegrationPlace{ID: "pl-3", ConnectionID: conn.ID, Kind: database.PlaceChat,
		ConversationID: "19:chat@thread.v2", Name: "Release chat"}
	if err := db.AddPlace(chat, "", database.RequestMeta{}); err != nil {
		t.Fatalf("AddPlace chat: %v", err)
	}
	places, err := db.ListPlaces(conn.ID)
	if err != nil || len(places) != 2 || places[0].Kind != database.PlaceTeam {
		t.Fatalf("ListPlaces = %+v, %v; want the team first, then the chat", places, err)
	}

	if err := db.AttachAppIntegration(&database.AppIntegration{
		AppID: app.ID, Kind: "teamsbot", ConnectionID: conn.ID, Enabled: true,
		Destination: database.Destination{ID: "channel:19:team@thread.tacv2|19:general", Label: "Toft Test › General"},
	}, "", database.RequestMeta{}); err != nil {
		t.Fatalf("AttachAppIntegration: %v", err)
	}
	apps, isDefault, err := db.PlaceInUse(conn.ID, "channel:19:team@thread.tacv2|")
	if err != nil || len(apps) != 1 || apps[0] != app.Slug || isDefault {
		t.Errorf("PlaceInUse(team) = %v, %v, %v; want the attached app only", apps, isDefault, err)
	}
	apps, _, err = db.PlaceInUse(conn.ID, "chat:19:chat@thread.v2")
	if err != nil || len(apps) != 0 {
		t.Errorf("PlaceInUse(chat) = %v, %v; nothing points into the chat", apps, err)
	}

	if err := db.DeletePlace(chat, "", database.RequestMeta{}); err != nil {
		t.Fatalf("DeletePlace: %v", err)
	}
	if err := db.DetachAppIntegration(app.ID, "teamsbot", "", database.RequestMeta{}); err != nil {
		t.Fatalf("DetachAppIntegration: %v", err)
	}
	if err := db.DeleteConnection(conn.ID, "", database.RequestMeta{}); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	if left, _ := db.ListPlaces(conn.ID); len(left) != 0 {
		t.Errorf("a deleted connection left %d places behind", len(left))
	}
}
