package client

import (
	"context"

	"github.com/mdenesfe/wax/internal/cryptoutil"
	"github.com/mdenesfe/wax/internal/wabin"
)

// csToken returns the <cstoken> WhatsApp clients attach to a 1:1 message
// when they hold no privacy token of the recipient (typically the first
// message to someone): HMAC-SHA256 keyed with our NCT salt over the
// recipient's LID. Without it a first message counts against the account's
// limit for reaching out to new people.
//
// The salt comes from the phone (history sync, nct_salt_sync app state). It
// returns nil when the salt or the recipient's LID is not known.
func (c *Client) csToken(ctx context.Context, to wabin.JID) []byte {
	to = to.ToNonAD()
	if to.Server != wabin.ServerLID || to.User == "0" { // "0": WhatsApp's own announcements
		return nil
	}
	salt, err := c.cfg.Store.GetNCTSalt(ctx, c.cfg.InstanceID)
	if err != nil {
		c.log.Debug("nct salt lookup failed", "error", err)
		return nil
	}
	if len(salt) == 0 {
		return nil
	}
	return cryptoutil.HMACSHA256(salt, []byte(to.String()))
}
