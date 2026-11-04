package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"strings"
)

// A setup code carries everything one device needs, so another device can be set up with a single paste.
const setupScheme = "mtc://"

type setupCode struct {
	DeploymentID string `json:"d"`
	AuthKey      string `json:"k"`
	Node         string `json:"n"`
	Backup       string `json:"b,omitempty"`
}

// SetupCode packs the mailbox, its key, the node and the backup route into one string.
func (s Settings) SetupCode() string {
	content, _ := json.Marshal(setupCode{s.DeploymentID, s.AuthKey, s.Node, s.Backup})
	return setupScheme + base64URL.EncodeToString(content)
}

func parseSetupCode(text string) (setupCode, error) {
	var code setupCode
	content, err := base64URL.DecodeString(strings.TrimPrefix(text, setupScheme))
	if err != nil {
		return code, err
	}
	if err := json.Unmarshal(content, &code); err != nil {
		return code, err
	}
	if code.DeploymentID == "" || code.AuthKey == "" {
		return code, errors.New("setup code is missing the mailbox")
	}
	return code, nil
}

var (
	webAppPath   = regexp.MustCompile(`/macros/(?:[^/]+/)?s/([A-Za-z0-9_-]{20,})/(exec|dev)\b`)
	deploymentID = regexp.MustCompile(`^[A-Za-z0-9_-]{20,}$`)
)

// ParseMailboxAddress returns the deployment ID in a Web app URL, or the text itself when it already is one.
func ParseMailboxAddress(text string) (string, error) {
	text = strings.TrimSpace(text)
	if deploymentID.MatchString(text) {
		return text, nil
	}
	match := webAppPath.FindStringSubmatch(text)
	if match == nil {
		return "", errors.New("not a Web app address")
	}
	// The /dev address only answers the script's owner while signed in, so it cannot serve as a mailbox.
	if match[2] == "dev" {
		return "", errors.New("a test deployment address")
	}
	return match[1], nil
}

// Errors from Setup are worded to be shown to the user as they are.
var (
	errBadSetupCode  = errors.New("That setup code is incomplete. Copy all of it again.")
	errBadAddress    = errors.New("That is not a Web app address. Copy the URL Google shows after deploying, the one ending in /exec.")
	errBadNode       = errors.New("That is not a valid node string.")
	errNoAnswer      = errors.New("Your mailbox did not answer. Check the address and try again.")
	errClaimed       = errors.New("This mailbox already belongs to another device. Paste the setup code from that device instead.")
	errOldScript     = errors.New("This mailbox runs an older Code.gs. Deploy the latest Code.gs and try again.")
	errNoRandomBytes = errors.New("Could not create a key on this device.")
)

// Setup fills in settings from what the user pasted: either a setup code, or a mailbox address and a node string.
// A new mailbox gets a random key; a mailbox the settings already hold keeps its key.
// The mailbox is checked before anything changes, and settings stay as they were when it fails.
func Setup(ctx context.Context, settings *Settings, mailbox, node string) error {
	updated := *settings
	mailbox = strings.TrimSpace(mailbox)
	claim := false
	if strings.HasPrefix(mailbox, setupScheme) {
		code, err := parseSetupCode(mailbox)
		if err != nil {
			return errBadSetupCode
		}
		updated.DeploymentID, updated.AuthKey, updated.Node, updated.Backup = code.DeploymentID, code.AuthKey, code.Node, code.Backup
	} else {
		id, err := ParseMailboxAddress(mailbox)
		if err != nil {
			return errBadAddress
		}
		if id != updated.DeploymentID || updated.AuthKey == "" {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return errNoRandomBytes
			}
			updated.DeploymentID, updated.AuthKey, claim = id, base64URL.EncodeToString(key), true
		}
		updated.Node = strings.TrimSpace(node)
	}
	if _, err := ParseNodeInfo(updated.Node); err != nil {
		return errBadNode
	}

	box := NewMailbox(updated.OwnMailbox())
	var err error
	if claim {
		err = box.Claim(ctx)
	} else {
		err = box.Ping(ctx)
	}
	switch {
	case errors.Is(err, ErrClaimed):
		return errClaimed
	case errors.Is(err, ErrCannotClaim):
		return errOldScript
	case err != nil:
		log.Printf("mailbox check: %v", err)
		return errNoAnswer
	}
	*settings = updated
	return nil
}
