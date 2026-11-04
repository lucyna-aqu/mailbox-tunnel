package tunnel

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

const testDeploymentID = "AKfyTESTdeployment_ID-0123456789"

func TestSetupCodeRoundTrip(t *testing.T) {
	settings := Settings{DeploymentID: testDeploymentID, AuthKey: "key", Node: "mtn://node", Backup: "mtn://relay", AutoConnect: true}
	code := settings.SetupCode()
	if !strings.HasPrefix(code, setupScheme) {
		t.Fatalf("code %q lacks the scheme", code)
	}
	parsed, err := parseSetupCode(code)
	if err != nil {
		t.Fatal(err)
	}
	want := setupCode{testDeploymentID, "key", "mtn://node", "mtn://relay"}
	if parsed != want {
		t.Fatalf("got %+v, want %+v", parsed, want)
	}
	if _, err := parseSetupCode(code[:len(code)-10]); err == nil {
		t.Fatal("a cut-off code was accepted")
	}
}

func TestParseMailboxAddress(t *testing.T) {
	for text, want := range map[string]string{
		testDeploymentID: testDeploymentID,
		"  https://script.google.com/macros/s/" + testDeploymentID + "/exec  ":           testDeploymentID,
		"https://script.google.com/macros/s/" + testDeploymentID + "/exec?x=1":           testDeploymentID,
		"https://script.google.com/a/macros/example.org/s/" + testDeploymentID + "/exec": testDeploymentID,
		"https://script.google.com/macros/s/" + testDeploymentID + "/dev":                "",
		"https://script.google.com/home/projects/" + testDeploymentID + "/edit":          "",
		"short": "",
		"https://script.google.com/macros/s/" + testDeploymentID + "/executions": "",
	} {
		got, err := ParseMailboxAddress(text)
		if want == "" && err == nil {
			t.Errorf("%q: accepted as %q", text, got)
		}
		if want != "" && (err != nil || got != want) {
			t.Errorf("%q: got %q, %v", text, got, err)
		}
	}
}

func TestSetupRejectsBadInputWithoutChangingSettings(t *testing.T) {
	saved := Settings{DeploymentID: testDeploymentID, AuthKey: "key", Node: "mtn://old"}
	for _, input := range [][2]string{
		{"mtc://not-base64!", ""},
		{"not an address", "mtn://x"},
		{"https://script.google.com/macros/s/" + testDeploymentID + "/exec", "not a node"},
	} {
		settings := saved
		if err := Setup(context.Background(), &settings, input[0], input[1]); err == nil {
			t.Errorf("%q accepted", input[0])
		}
		if !reflect.DeepEqual(settings, saved) {
			t.Errorf("%q changed the settings to %+v", input[0], settings)
		}
	}
}
