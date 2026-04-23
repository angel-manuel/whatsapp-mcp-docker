package cache

import (
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func buildContactEvent(jid types.JID, fullName, firstName string) *events.Contact {
	return &events.Contact{
		JID: jid,
		Action: &waSyncAction.ContactAction{
			FullName:  proto.String(fullName),
			FirstName: proto.String(firstName),
		},
	}
}
