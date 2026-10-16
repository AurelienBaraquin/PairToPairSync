// Package pairing makes connecting devices as simple as scanning a QR code.
//
// An invitation is a short-lived, single-use credential. It carries what a new
// device needs to find and recognise the inviter (its ID and addresses) plus a
// random token proving the scanner is the intended guest. Everything sensitive
// (the folder secret) travels afterwards, inside the authenticated and
// encrypted connection, and only in reply to a valid token.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
)

// Scheme prefixes the text form of an invitation, so that a phone app can
// register as the handler of p2psync:// links.
const Scheme = "p2psync://pair/"

// DefaultTTL is how long an invitation stays valid.
const DefaultTTL = 10 * time.Minute

// Invite is the content of the QR code. Field names are short to keep the code small.
type Invite struct {
	V     int      `json:"v"`
	ID    string   `json:"id"`          // inviter device ID
	Name  string   `json:"n,omitempty"` // inviter display name
	Addrs []string `json:"a,omitempty"` // inviter multiaddrs (without /p2p/ID)
	Token string   `json:"t"`           // single-use secret, base64url
	// Folder the guest is invited to.
	Folder       string `json:"f"`
	Label        string `json:"l,omitempty"`
	InviterRelay bool   `json:"ir,omitempty"` // the inviter is a relay for this folder
	InviteeRelay bool   `json:"er,omitempty"` // the guest will be a relay for this folder
	Exp          int64  `json:"x"`            // unix seconds

	// Joining the inviter's whole network (version 2): the inviter, a full device,
	// introduces the guest in Folders (which are only shown to the guest; the real
	// list is in the signed introduction it receives). Approve: the inviter's owner confirms each request by hand, so the guest
	// has to wait for an answer.
	Net     bool     `json:"nw,omitempty"`
	Folders []string `json:"fs,omitempty"`
	Approve bool     `json:"ap,omitempty"`
}

// MaxApprovalWait is how long a guest waits for the inviter's owner to approve its request.
const MaxApprovalWait = 3 * time.Minute

// NewToken returns a fresh random token.
func NewToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Encode renders the invitation as text (what the QR code contains).
func (i Invite) Encode() string {
	b, _ := json.Marshal(i)
	return Scheme + base64.RawURLEncoding.EncodeToString(b)
}

// Decode parses and validates the text form (the prefix is optional so that
// the bare payload can be pasted too).
func Decode(s string) (Invite, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, Scheme)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Invite{}, errors.New("not a valid invitation")
	}
	var i Invite
	if err := json.Unmarshal(raw, &i); err != nil {
		return Invite{}, errors.New("not a valid invitation")
	}
	if i.V != 1 && i.V != 2 {
		return Invite{}, fmt.Errorf("unsupported invitation version %d (update p2psync)", i.V)
	}
	if i.V == 1 && (i.Net || i.Approve || len(i.Folders) > 0) {
		return Invite{}, errors.New("malformed invitation")
	}
	if id, err := identity.Normalize(i.ID); err != nil {
		return Invite{}, err
	} else {
		i.ID = id
	}
	if i.Token == "" || len(i.Token) > 64 || len(i.Addrs) > 8 {
		return Invite{}, errors.New("malformed invitation")
	}
	for _, a := range i.Addrs {
		if len(a) > 300 {
			return Invite{}, errors.New("malformed invitation")
		}
	}
	// Names and IDs come from whoever made the QR code: treat them as hostile.
	if i.Net {
		if len(i.Folders) == 0 || len(i.Folders) > 64 || i.InviterRelay {
			return Invite{}, errors.New("malformed invitation")
		}
		for _, f := range i.Folders {
			if !config.ValidFolderID(f) {
				return Invite{}, errors.New("the invitation names an invalid folder")
			}
		}
		i.Folder = i.Folders[0]
	}
	if !config.ValidFolderID(i.Folder) {
		return Invite{}, errors.New("the invitation names an invalid folder")
	}
	i.Name, i.Label = config.Clean(i.Name), config.Clean(i.Label)
	if i.InviterRelay && i.InviteeRelay {
		return Invite{}, errors.New("a relay cannot invite another relay")
	}
	return i, nil
}

// Expired reports whether the invitation's validity has passed.
func (i Invite) Expired(now time.Time) bool { return now.Unix() > i.Exp }

// TokenKey is what the inviter stores instead of the token itself.
func TokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
