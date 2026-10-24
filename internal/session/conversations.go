package session

// CONVERSATIONS (R4, plan section 4).
//
// The wrist channel carries threads: "main" (the primary channel: the
// wrist, Telegram, the recap; exactly as before) and iOS side
// conversations. Every side conversation is served by ONE side session per
// instance (chat key SideChat, lane side), so main and side turns share the
// workspace and run strictly one after the other under the per-workspace
// interactive lock. Whether a side turn starts fresh is decided inside
// RunTurn (TurnRequest.Side), never here.

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/kjhnns/agentd/internal/channel"
)

// SideChat is the chat key of the side session. It never collides with a
// main key (a Telegram chat id, or "watch").
const SideChat = "watch-side"

// RouteConversation routes one wrist message by its conversation: main (or
// none) exactly as RouteInbound; any other id to the side session, with the
// conversation's own transcript attached as an artifact on a fresh start
// into a thread that already has messages. ContextConv attaches another
// conversation (main included) to the turn as an artifact.
func (m *Manager) RouteConversation(ctx context.Context, ch channel.Adapter, in channel.InboundMsg, mainChats, sideChats *ChatMap, cwd, model string, exp channel.ConvExporter) error {
	conv := in.Conv
	if conv == "" {
		conv = ConvMain
	}
	opts := routeOpts{conv: conv}
	chats := mainChats
	if conv != ConvMain {
		if sideChats == nil || sideChats.Lane() != LaneSide {
			return fmt.Errorf("session: conversation %s needs a side session map", conv)
		}
		chats = sideChats
		in.UserID = SideChat
		opts.side = true
		msgID := in.MsgID
		opts.onFresh = func() (string, []TurnAttachment, error) {
			return exportAttachment(exp, conv, msgID)
		}
	}
	if cc := in.ContextConv; cc != "" {
		msgID := in.MsgID
		opts.prepare = func() (string, []TurnAttachment, error) {
			return exportAttachment(exp, cc, msgID)
		}
	}
	return m.routeInbound(ctx, ch, in, chats, cwd, model, opts)
}

// exportAttachment exports conv as an artifact for the message msgID and
// records it on the message. Nothing to export = no pointer.
func exportAttachment(exp channel.ConvExporter, conv, msgID string) (string, []TurnAttachment, error) {
	if exp == nil {
		return "", nil, errors.New("this channel cannot export conversations")
	}
	art, ok, err := exp.ExportConv(conv, msgID)
	if err != nil {
		return "", nil, fmt.Errorf("export of conversation %s: %w", conv, err)
	}
	if !ok {
		return "", nil, nil
	}
	if msgID != "" {
		if err := exp.NoteAttachment(msgID, art); err != nil {
			// The turn record carries the attachment; the store row only
			// shows it to the app.
			log.Printf("session: attachment of %s on %s not stored: %v", conv, msgID, err)
		}
	}
	return art.Pointer, []TurnAttachment{{Kind: "conv", ID: conv, Path: art.Path, Name: art.Title}}, nil
}
