package notify

import (
	"strings"
	"testing"
)

// BB-8: a success posted on app_event is not "broken" and carries no failure footer.
func TestAppEventInfoPostIsGoodNews(t *testing.T) {
	m := Templates["app_event"].Render(TemplateData{AppName: "a", EventTitle: `The job "x" succeeded`, Severity: "info"}, "")
	if strings.Contains(m.Title, "broken") || strings.Contains(m.Footer, "failure") {
		t.Errorf("message = %+v", m)
	}
}
