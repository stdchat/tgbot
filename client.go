// Copyright (C) 2020 Christopher E. Miller
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package tgbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"golang.org/x/time/rate"
	"stdchat.org"
	"stdchat.org/service"
)

const Protocol = "tgbot"

type Client struct {
	sendQueue chan<- func() // use enqueue
	svc       *service.Service
	tp        service.Transporter
	bot       *tgbotapi.BotAPI
	groups    []*Group // locked by mx
	mx        sync.RWMutex
	done      chan struct{}
	botToken  string
	fileURL   string
	lim1      *rate.Limiter
	lim2      *rate.Limiter
	fileLim   *rate.Limiter // http files has a limit so we don't hammer or interrupt chat.
	state     int32         // atomic: state*
}

const (
	stateInit = iota
	stateStarted
	stateReady // started+ready
	stateClosed
)

func (client *Client) getState() int32 {
	return atomic.LoadInt32(&client.state)
}

func (client *Client) GetGroups() []*Group {
	client.mx.RLock()
	defer client.mx.RUnlock()
	return append([]*Group(nil), client.groups...)
}

// Returns true if newly added.
// Does not add if addType is empty.
func (client *Client) getOrAddGroup(info stdchat.EntityInfo, addType string) (*Group, bool) {
	if addType == "" {
		client.mx.RLock()
		defer client.mx.RUnlock()
	} else {
		client.mx.Lock()
		defer client.mx.Unlock()
	}
	for _, group := range client.groups {
		if group.Info.ID == info.ID {
			return group, false
		}
	}
	if addType != "" {
		group := &Group{client: client, Info: info, typ: addType}
		client.groups = append(client.groups, group)
		return group, true
	}
	return nil, false
}

// Returns nil if not in the group/channel.
func (client *Client) GetGroup(id string) *Group {
	info := stdchat.EntityInfo{}
	info.ID = id
	group, _ := client.getOrAddGroup(info, "")
	return group
}

func (client *Client) removeGroup(group *Group) bool {
	client.mx.Lock()
	defer client.mx.Unlock()
	for i, g := range client.groups {
		if g == group {
			ilast := len(client.groups) - 1
			client.groups[i], client.groups[ilast] = client.groups[ilast], nil
			client.groups = client.groups[:ilast]
			return true
		}
	}
	return false
}

func (client *Client) Logout(reason string) error {
	if !atomic.CompareAndSwapInt32(&client.state, stateStarted, stateClosed) &&
		!atomic.CompareAndSwapInt32(&client.state, stateReady, stateClosed) {
		if client.getState() == stateInit {
			return errors.New("not started")
		}
		return errors.New("already closed")
	}
	client.bot.StopReceivingUpdates()
	close(client.done)
	client.tp.StopServeURL(client.NetworkID(), client.fileURL)
	return nil
}

func (client *Client) Close() error {
	return client.Logout("Close")
}

func (client *Client) Start(ctx context.Context, id string) error {
	if !atomic.CompareAndSwapInt32(&client.state, stateInit, stateStarted) {
		return errors.New("already started")
	}

	bot, err := tgbotapi.NewBotAPI(client.botToken)
	if err != nil {
		return err
	}

	fileURL, err := client.tp.ServeURL(client.NetworkID(), "file/",
		http.HandlerFunc(client.serveFile))
	if err != nil {
		return err
	}
	client.fileURL = fileURL

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		return err
	}
	client.bot = bot

	{
		msg := &stdchat.NetMsg{}
		msg.Init(service.MakeID(id), "online", Protocol,
			client.NetworkID())
		client.tp.Publish(msg.Network.ID, "", "network", msg)
	}

	go client.processUpdates(updates)
	return nil
}

func (client *Client) NetworkID() string {
	return "telegram.com"
}

func (client *Client) NetworkName() string {
	return "Telegram"
}

func (client *Client) ConnID() string {
	return "" // not applicable
}

type doneCtx struct {
	context.Context
	done <-chan struct{}
}

func (ctx *doneCtx) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *doneCtx) Err() error {
	select {
	case <-ctx.done:
		return errors.New("Client is done")
	default:
		return nil
	}
}

func (ctx *doneCtx) String() string {
	return "Client context: " + Protocol
}

func (client *Client) Context() context.Context {
	return &doneCtx{context.Background(), client.done}
}

func (client *Client) Closed() bool {
	return client.getState() == stateClosed
}

func (client *Client) Ready() bool {
	return client.getState() == stateReady
}

func (client *Client) setFeatures(values *stdchat.ValuesInfo) {
	// Add various network-specific values:
	values.Set("msg-multiline", "1")
	values.Set("msg-max-length", "4096") // From telegram bot api doc.
	values.Add("msg-type", "text/html")
	values.Add("msg-type", "text/markdown")
	values.Add("msg-type", "text/plain")
}

func (client *Client) serveFile(w http.ResponseWriter, r *http.Request) {
	ctx := client.Context()
	r = r.WithContext(ctx)
	ifile := strings.Index(r.URL.Path, "/file/")
	if ifile != -1 {
		fileID := r.URL.Path[ifile+6:]
		client.fileLim.Wait(ctx)
		if err := client.syncRequest(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		finfo, err := client.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fr, err := http.Get(finfo.Link(client.botToken))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer fr.Body.Close()
		if fr.StatusCode != 200 {
			http.Error(w, "Received "+fr.Status, http.StatusBadGateway)
			return
		}
		contentType := fr.Header.Get("Content-Type")
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		strContentLength := fr.Header.Get("Content-Length")
		if strContentLength != "" {
			w.Header().Set("Content-Length", strContentLength)
		}
		w.WriteHeader(http.StatusOK)
		io.Copy(w, fr.Body)
	} else {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	}
}

func (client *Client) processUpdates(updates tgbotapi.UpdatesChannel) {
	atomic.CompareAndSwapInt32(&client.state, stateStarted, stateReady)
	{
		msg := &stdchat.NetMsg{}
		msg.Init(service.MakeID(""), "ready", Protocol, client.NetworkID())
		msg.Network.SetName(client.NetworkName(), "")
		client.setFeatures(&msg.Values)
		client.tp.Publish(msg.Network.ID, "", "network", msg)
	}

	for {
		select {
		case <-client.done:
			// Do some finishing things when done.
			// This is a good place for this because it's after all other events.
			// Send unsubscribe for all groups I'm in.
			for _, group := range client.GetGroups() {
				client.publishUnsubscribe(group)
			}
			{
				msg := &stdchat.NetMsg{}
				msg.Init(service.MakeID(""), "offline", Protocol,
					client.NetworkID())
				msg.Network.SetName(client.NetworkName(), "")
				client.tp.Publish(msg.Network.ID, "", "network", msg)
			}
			return

		case update, ok := <-updates:
			if !ok {
				return
			}
			client.tgbotEvent(update)
		}
	}
}

func strint64(x int64) string {
	return strconv.FormatInt(x, 10)
}

func strint(x int) string {
	return strint64(int64(x))
}

func strid64(id int64) string {
	return strint64(id)
}

func strid(id int) string {
	return strid64(int64(id))
}

func getUserID(user *tgbotapi.User) string {
	if user == nil {
		return ""
	}
	if user.UserName != "" {
		return user.UserName
	}
	return strid(user.ID)
}

func setUser(ent *stdchat.EntityInfo, user *tgbotapi.User) {
	if user == nil {
		return
	}
	ent.Init(getUserID(user), "user")
	name := ""
	if user.UserName != "" {
		name = user.UserName
	}
	dispName := user.FirstName
	if user.LastName != "" {
		dispName += " " + user.LastName
	}
	ent.SetName(name, dispName)
}

func (client *Client) isFromMyself(m *tgbotapi.Message) bool {
	return m.From != nil && m.From.ID == client.bot.Self.ID
}

// returns nil if it's not a known message type.
func (client *Client) tgbotNewChatMsg(m *tgbotapi.Message, typeSpecial string) (
	chatMsg *stdchat.ChatMsg, node string) {
	// https://core.telegram.org/bots/api#message
	// https://core.telegram.org/bots/api#available-methods

	// TODO: how to detect sendMediaGroup?
	// TODO: how to detect sendPoll?
	// How to detect sendChatAction? maybe bots don't get this?

	isChannel := m.Chat.Type == "channel"
	msg := &stdchat.ChatMsg{}
	msg.Init(strid(m.MessageID), "", Protocol, client.NetworkID())
	msg.Network.SetName(client.NetworkName(), "")
	msg.Time = m.Time()
	if m.ReplyToMessage != nil {
		msg.ReplyToID = strid(m.ReplyToMessage.MessageID)
	}
	chatType := "group"
	if m.Chat.Type == "private" {
		chatType = "private"
	}
	msg.Destination.Init(strid64(m.Chat.ID), chatType)
	if m.Chat.FirstName != "" || m.Chat.LastName != "" {
		msg.Destination.SetName(strings.TrimSpace(
			m.Chat.FirstName+" "+m.Chat.LastName), "")
	} else {
		msg.Destination.SetName(m.Chat.Title, "")
	}
	tgbotCmd := m.Command()
	if tgbotCmd != "" {
		msg.Values.Set("tgbot.command", tgbotCmd)
		msg.Values.Set("tgbot.command-args", m.CommandArguments())
	}
	if m.From != nil {
		setUser(&msg.From, m.From)
	}
	if m.Entities != nil {
		entcounts := map[string]int{}
		var u2 []uint16 // Need UTF-16 code units for entity offsets/lengths.
		for _, ent := range *m.Entities {
			entcounts[ent.Type]++
			count := entcounts[ent.Type]
			getkey := func() string {
				key := "tgbot." + ent.Type
				if count != 1 {
					key += "." + strint(count)
				}
				return key
			}
			switch ent.Type {
			case "bot_command":
				// Ignore since we have tgbot.command above.
				// bot_command is more broad and adds some complexity.
			case "bold", "italic", "code", "pre":
				// Ignore formatting for this.
				// TODO: convert to HTML, use msg.Message.Set("text/html", ...)
			case "mention", "hashtag", "cashtag", "email", "phone_number":
				if u2 == nil {
					u2 = utf16.Encode([]rune(m.Text))
				}
				msg.Values.Set(getkey(),
					string(utf16.Decode(u2[ent.Offset:ent.Offset+ent.Length])))
			case "text_mention":
				msg.Values.Set(getkey(), getUserID(ent.User))
			case "text_link":
				msg.Values.Set(getkey(), ent.URL)
			}
		}
	}

	base := "msg"
	if isChannel {
		base = "info"
	}
	tgbotType := ""
	if m.Photo != nil {
		tgbotType = "sendPhoto"
		for _, x := range *m.Photo {
			mi := stdchat.MediaInfo{}
			mi.Init("image/*", client.fileURL+x.FileID)
			mi.Values.Set("width", strint(x.Width))
			mi.Values.Set("height", strint(x.Height))
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.Audio != nil {
		tgbotType = "sendAudio"
		{
			x := m.Audio
			mi := stdchat.MediaInfo{}
			mi.Init("audio/*", client.fileURL+x.FileID)
			if x.MimeType != "" {
				mi.Type = x.MimeType
			}
			mi.Name = x.Performer
			if x.Title != "" {
				if mi.Name != "" {
					mi.Name += " - "
				}
				mi.Name += x.Title
			}
			mi.Values.Set("duration", strint(x.Duration)) // secs
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.Document != nil {
		tgbotType = "sendDocument"
		{
			x := m.Document
			mi := stdchat.MediaInfo{}
			mi.Init("*/*", client.fileURL+x.FileID)
			if x.MimeType != "" {
				mi.Type = x.MimeType
			}
			if x.Thumbnail != nil {
				mi.ThumbURL = client.fileURL + x.Thumbnail.FileID
			}
			mi.Name = x.FileName
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.Video != nil {
		tgbotType = "sendVideo"
		{
			x := m.Video
			mi := stdchat.MediaInfo{}
			mi.Init("video/*", client.fileURL+x.FileID)
			if x.MimeType != "" {
				mi.Type = x.MimeType
			}
			//mi.Name = ???
			if x.Thumbnail != nil {
				mi.ThumbURL = client.fileURL + x.Thumbnail.FileID
			}
			mi.Values.Set("width", strint(x.Width))
			mi.Values.Set("height", strint(x.Height))
			mi.Values.Set("duration", strint(x.Duration)) // secs
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.Animation != nil {
		tgbotType = "sendAnimation"
		{
			x := m.Animation
			mi := stdchat.MediaInfo{}
			mi.Init("*/*", client.fileURL+x.FileID)
			if x.MimeType != "" {
				mi.Type = x.MimeType
			}
			//mi.Name = ???
			if x.Thumbnail != nil {
				mi.ThumbURL = client.fileURL + x.Thumbnail.FileID
			}
			mi.Values.Set("width", strint(x.Width))
			mi.Values.Set("height", strint(x.Height))
			mi.Values.Set("duration", strint(x.Duration)) // secs
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.Voice != nil {
		tgbotType = "sendVoice"
		{
			x := m.Voice
			mi := stdchat.MediaInfo{}
			mi.Init("audio/*", client.fileURL+x.FileID)
			if x.MimeType != "" {
				mi.Type = x.MimeType
			}
			//mi.Name = ???
			mi.Values.Set("duration", strint(x.Duration)) // secs
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.VideoNote != nil {
		tgbotType = "sendVideoNote"
		{
			x := m.VideoNote
			mi := stdchat.MediaInfo{}
			mi.Init("video/mp4", client.fileURL+x.FileID)
			//mi.Name = ???
			if x.Thumbnail != nil {
				mi.ThumbURL = client.fileURL + x.Thumbnail.FileID
			}
			mi.Values.Set("width", strint(x.Length))
			mi.Values.Set("height", strint(x.Length))
			mi.Values.Set("duration", strint(x.Duration)) // secs
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Caption)
	} else if m.Venue != nil {
		base = "other"
		tgbotType = "sendVenue"
		j, _ := stdchat.JSON.Marshal(m.Venue)
		if j != nil {
			msg.Message.Set("application/x-tgbot-venue+json", string(j))
		}
	} else if m.Sticker != nil {
		tgbotType = "sendSticker"
		{
			x := m.Sticker
			mi := stdchat.MediaInfo{}
			mi.Init("image/*", client.fileURL+x.FileID)
			mi.Name = string(x.SetName + " " + x.Emoji)
			if x.Thumbnail != nil {
				mi.ThumbURL = client.fileURL + x.Thumbnail.FileID
			}
			mi.Values.Set("width", strint(x.Width))
			mi.Values.Set("height", strint(x.Height))
			if x.FileSize != 0 {
				mi.Values.Set("file-size", strint(x.FileSize))
			}
			msg.Attachments = append(msg.Attachments, mi)
		}
		msg.Message.SetText(m.Sticker.Emoji)
	} else if m.Invoice != nil {
		base = "other"
		tgbotType = "sendInvoice"
		j, _ := stdchat.JSON.Marshal(m.Invoice)
		if j != nil {
			msg.Message.Set("application/x-tgbot-invoice+json", string(j))
		}
	} else if m.Game != nil {
		base = "other"
		tgbotType = "sendGame"
		j, _ := stdchat.JSON.Marshal(m.Game)
		if j != nil {
			msg.Message.Set("application/x-tgbot-game+json", string(j))
		}
	} else if m.Text != "" {
		tgbotType = "sendMessage"
		msg.Message.SetText(m.Text)
	} else if m.Contact != nil {
		base = "other"
		tgbotType = "sendContact"
		j, _ := stdchat.JSON.Marshal(m.Contact)
		if j != nil {
			msg.Message.Set("application/x-tgbot-contact+json", string(j))
		}
	} else if m.Location != nil {
		base = "other"
		tgbotType = "sendLocation"
		j, _ := stdchat.JSON.Marshal(m.Location)
		if j != nil {
			msg.Message.Set("application/x-tgbot-location+json", string(j))
		}
	}

	if tgbotType == "" {
		return nil, ""
	}

	typeSpecialInsert := strings.TrimSuffix("/"+typeSpecial, "/")
	if m.ForwardFromMessageID != 0 {
		msg.Type = base + typeSpecialInsert + "/forward/tgbot." + tgbotType
		msg.Values.Set("tgbot.forward_from_message_id",
			strid(m.ForwardFromMessageID))
		msg.Values.Set("tgbot.forward_from", getUserID(m.ForwardFrom))
		msg.Values.Set("tgbot.forward_from_chat", strid64(m.ForwardFromChat.ID))
		msg.Values.Set("tgbot.forward_date", strint(m.ForwardDate))
	} else {
		msg.Type = base + typeSpecialInsert + "/tgbot." + tgbotType
	}

	node = base
	if client.isFromMyself(m) {
		node = base + "-out"
	}

	return msg, node
}

func (client *Client) publishSubscribe(group *Group) {
	msg := &stdchat.SubscribeMsg{}
	msg.Init(service.MakeID(""), "subscribe", Protocol, client.NetworkID())
	msg.Network.Name = client.NetworkName()
	msg.Destination = group.Info
	msg.Subject.SetText(group.GetTitle())
	setUser(&msg.Myself, &client.bot.Self)
	msg.Members = group.GetMembersInfo()
	photoURL, photoThumbURL := group.GetPhoto()
	if photoURL != "" {
		msg.Photo.Init("image/*", photoURL)
		msg.Photo.ThumbURL = photoThumbURL
	}
	msg.Values.Set("tgbot.chat-type", group.GroupType())
	client.tp.Publish(msg.Network.ID, msg.Destination.ID, "subscribe", msg)
}

func (client *Client) publishUnsubscribe(group *Group) {
	msg := &stdchat.SubscribeMsg{}
	msg.Init(service.MakeID(""), "unsubscribe", Protocol, client.NetworkID())
	msg.Network.Name = client.NetworkName()
	msg.Destination = group.Info
	//msg.Subject.SetText(group.GetTitle())
	setUser(&msg.Myself, &client.bot.Self)
	msg.Values.Set("tgbot.chat-type", group.GroupType())
	client.tp.Publish(msg.Network.ID, msg.Destination.ID, "unsubscribe", msg)
}

func (client *Client) updateGroup(group *Group, chatID int64) error {
	client.syncRequest(client.Context())
	chat, err := client.bot.GetChat(tgbotapi.ChatConfig{ChatID: chatID})
	if err != nil {
		return err
	}
	group.setTitle(chat.Title)
	photoURL := ""
	photoThumbURL := ""
	if chat.Photo.BigFileID != "" {
		photoURL = client.fileURL + chat.Photo.BigFileID
	}
	if chat.Photo.SmallFileID != "" {
		photoThumbURL = client.fileURL + chat.Photo.SmallFileID
	}
	group.setPhoto(photoURL, photoThumbURL)

	// Use getChatAdministrators to get admin members (non bots).
	// These users may or may not actually be in the group right now, I think?
	// Should be fine to assume they're present...
	client.syncRequest(client.Context())
	admins, _ := client.bot.GetChatAdministrators(tgbotapi.ChatConfig{ChatID: chatID})
	for _, am := range admins {
		var member Member
		setUser(&member.User, am.User)
		group.addMember(member)
	}
	return nil
}

func (client *Client) publishMsg(m *tgbotapi.Message, typeSpecial string) bool {
	msg, node := client.tgbotNewChatMsg(m, typeSpecial)
	if msg == nil {
		return false
	}

	var group *Group // nil if not considered a group to us.
	if m.Chat.Type != "channel" && m.Chat.Type != "private" {

		// Handle migrations.
		// MigrateToChatID = group has been migrated to a supergroup
		// MigrateFromChatID = supergroup has been migrated from a group
		if m.MigrateToChatID != 0 || m.MigrateFromChatID != 0 {
			oldGroup := client.GetGroup(msg.Destination.ID)
			if oldGroup != nil {
				client.removeGroup(oldGroup)
				client.publishUnsubscribe(oldGroup)
			}
			newDest := stdchat.EntityInfo{}
			// Note: newDest type is always "group" per stdchat.
			if m.MigrateToChatID != 0 {
				newDest.Init(strid64(m.MigrateToChatID), "group")
			} else {
				newDest.Init(strid64(m.MigrateFromChatID), "group")
			}
			// Specify the "group" type within tgbot:
			group, _ = client.getOrAddGroup(newDest, "supergroup")
			if oldGroup != nil {
				group.migrateFrom(oldGroup)
			}
			var myselfMember Member
			setUser(&myselfMember.User, &client.bot.Self)
			group.addMember(myselfMember)
			client.publishSubscribe(group)
			msg.Destination = newDest
		}

		var groupAdded bool
		group, groupAdded = client.getOrAddGroup(msg.Destination, m.Chat.Type)
		if groupAdded {
			// New group, subscribed!
			var myselfMember Member
			setUser(&myselfMember.User, &client.bot.Self)
			group.addMember(myselfMember)
			client.updateGroup(group, m.Chat.ID)
			client.publishSubscribe(group)
		}
		publishEnter := func(member Member) {
			msg := &stdchat.EnterMsg{}
			msg.Init(service.MakeID(""), "group-enter", Protocol, client.NetworkID())
			msg.Network.Name = client.NetworkName()
			msg.Destination = group.Info
			msg.Member.Type = "member"
			msg.Member.Info.User = member.User
			client.tp.Publish(msg.Network.ID, msg.Destination.ID, "group", msg)
		}
		if msg.From.ID != "" {
			var member Member
			member.User = msg.From
			if group.addMember(member) {
				publishEnter(member)
			}
		}
		if m.NewChatMembers != nil {
			for _, x := range *m.NewChatMembers {
				if x.ID != client.bot.Self.ID { // Skip if myself, handled above.
					var member Member
					setUser(&member.User, &x)
					if group.addMember(member) {
						publishEnter(member)
					}
				}
			}
		}
	}

	client.tp.Publish(client.NetworkID(), msg.Destination.ID, node, msg)

	if m.LeftChatMember != nil && group != nil {
		// Group leave or unsubscribe!
		// Note: also publishing even if we didn't know the user was there.
		if m.LeftChatMember.ID == client.bot.Self.ID {
			// Remove group:
			client.removeGroup(group)
			// Publish:
			client.publishUnsubscribe(group)
		} else {
			// Remove them:
			group.removeMember(strid(m.LeftChatMember.ID))
			{ // Publish:
				msg := &stdchat.LeaveMsg{}
				msg.Init(service.MakeID(""), "group-leave", Protocol, client.NetworkID())
				msg.Network.Name = client.NetworkName()
				msg.Destination = group.Info
				setUser(&msg.User, m.LeftChatMember)
				client.tp.Publish(msg.Network.ID, msg.Destination.ID, "group", msg)
			}
		}

	}

	return true
}

func (client *Client) tgbotEvent(update tgbotapi.Update) {
	if update.Message != nil {
		client.publishMsg(update.Message, "")
	} else if update.EditedMessage != nil {
		// https://core.telegram.org/bots/api#updating-messages
		// editMessageText, editMessageCaption ...
		client.publishMsg(update.EditedMessage, "edit")
	} else if update.ChannelPost != nil {
		client.publishMsg(update.ChannelPost, "tgbot.channel_post")
	} else if update.EditedChannelPost != nil {
		client.publishMsg(update.EditedChannelPost, "edit/tgbot.channel_post")
	} else {
		// ...
	}
}

func tgbotBaseChat(msg *stdchat.ChatMsg, isChannel bool) (tgbotapi.BaseChat, error) {
	config := tgbotapi.BaseChat{}

	if msg.Destination.ID == "" {
		return config, errors.New("no destination specified")
	}
	if isChannel {
		if msg.Destination.ID[0] != '@' {
			return config, errors.New("invalid destination")
		}
		config.ChannelUsername = msg.Destination.ID
	} else {
		chatID, err := strconv.ParseInt(msg.Destination.ID, 10, 64)
		if err != nil {
			return config, errors.New("invalid destination")
		}
		config.ChatID = chatID
	}

	if msg.ReplyToID != "" {
		if isChannel {
			return config, errors.New("invalid reply to ID")
		} else {
			rtid, err := strconv.ParseInt(msg.ReplyToID, 10, 64)
			if err != nil {
				return config, errors.New("invalid reply to ID")
			}
			config.ReplyToMessageID = int(rtid)
		}
	}

	if s := msg.Values.Get("tgbot.reply_markup"); s != "" {
		config.ReplyMarkup = json.RawMessage(s)
	}

	if msg.Values.Get("tgbot.disable_notification") == "1" {
		config.DisableNotification = true
	}

	return config, nil
}

func tgbotMsgConfig(msg *stdchat.ChatMsg, isChannel bool) (tgbotapi.MessageConfig, error) {
	config := tgbotapi.MessageConfig{}

	var err error
	config.BaseChat, err = tgbotBaseChat(msg, isChannel)
	if err != nil {
		return config, err
	}

	// TODO: ensure the html & markdown used is allowed by tgbot...
	if t := msg.Message.Get("text/html").Content; t != "" {
		config.Text = t
		config.ParseMode = "html"
	} else if t := msg.Message.Get("text/markdown").Content; t != "" {
		config.Text = t
		config.ParseMode = "markdown"
	} else {
		text := msg.Message.String()
		if text == "" {
			for _, m := range msg.Message {
				if strings.HasPrefix(m.Type, "text/") {
					text = fmt.Sprintf("[%s] %s", m.Type, m.Content)
					break
				} else if text == "" {
					text = fmt.Sprintf("[%s]", m.Type)
				}
			}
			if text == "" {
				return config, errors.New("no message content specified")
			}
		}
		config.Text = text
	}

	if msg.Values.Get("tgbot.disable_web_page_preview") == "1" {
		config.DisableWebPagePreview = true
	}

	return config, nil
}

func (client *Client) tgbotStickerConfig(msg *stdchat.ChatMsg, isChannel bool) (tgbotapi.StickerConfig, error) {
	config := tgbotapi.StickerConfig{}

	var err error
	config.BaseChat, err = tgbotBaseChat(msg, isChannel)
	if err != nil {
		return config, err
	}

	if len(msg.Attachments) < 1 {
		return config, errors.New("no sticker specified")
	}
	a := msg.Attachments[0]

	if len(a.URL) > len(client.fileURL) && strings.HasPrefix(a.URL, client.fileURL) {
		fileID := a.URL[len(client.fileURL):]
		config.FileID = fileID
		config.UseExisting = true
	} else {
		u, err := url.Parse(a.URL)
		if err != nil {
			return config, err
		}
		config.File = u
	}

	if !strings.HasSuffix(a.Type, "*") {
		config.MimeType = a.Type
	}

	if sfileSize := a.Values.Get("file-size"); sfileSize != "" {
		fileSize, _ := strconv.ParseInt(sfileSize, 10, 64)
		if fileSize > 0 {
			config.FileSize = int(fileSize)
		}
	}

	return config, nil
}

func (client *Client) enqueueSend(c tgbotapi.Chattable, isEdit bool, reqID string) error {
	return client.enqueue(func() {
		m, err := client.bot.Send(c)
		if err != nil {
			client.tp.PublishError(service.MakeID(reqID), client.NetworkID(), err)
		} else {
			client.publishMsg(&m, getTypeSpecial(m.Chat.Type, isEdit))
		}
	})
}

func (client *Client) Handler(msg *stdchat.ChatMsg) {
	if msg.Network.ID == "" {
		panic("network ID")
	}

	switch msg.Type {
	case "msg", "msg/tgbot.sendMessage":
		config, err := tgbotMsgConfig(msg, false)
		if err != nil {
			client.tp.PublishError(service.MakeID(msg.ID), msg.Network.ID, err)
		} else {
			client.enqueueSend(config, false, msg.ID)
		}

	case "msg/tgbot.sendSticker":
		config, err := client.tgbotStickerConfig(msg, false)
		if err != nil {
			client.tp.PublishError(service.MakeID(msg.ID), msg.Network.ID, err)
		} else {
			client.enqueueSend(config, false, msg.ID)
		}

	//case "msg/action":
	// not to be confused with sendChatAction!
	// This is a message action, not some abstract action.

	//case "info":

	//case "other/tgbot.leaveChat": // TODO: ...

	default:
		client.tp.PublishError(service.MakeID(msg.ID), msg.Network.ID,
			errors.New("unhandled message of type "+msg.Type))
	}
}

func getTypeSpecial(tgbotChatType string, isEdit bool) string {
	typeSpecial := ""
	isChannel := tgbotChatType == "channel"
	if isEdit && isChannel {
		typeSpecial = "edit/tgbot.channel_post"
	} else if isEdit {
		typeSpecial = "edit"
	} else if isChannel {
		typeSpecial = "tgbot.channel_post"
	}
	return typeSpecial
}

func (client *Client) enqueueRaw(method string, values url.Values, reqID string) error {
	return client.enqueue(func() {
		resp, err := client.bot.MakeRequest(method, values)
		if err != nil {
			client.tp.PublishError(service.MakeID(reqID), client.NetworkID(), err)
		} else {
			m := &tgbotapi.Message{}
			if stdchat.JSON.Unmarshal(resp.Result, m) == nil {
				published := false
				// Check non-optional fields of Message to check if it is one.
				if m.MessageID != 0 && m.Chat != nil {
					isEdit := strings.HasPrefix(method, "editMessage")
					published = client.publishMsg(m, getTypeSpecial(m.Chat.Type, isEdit))
				}
				if !published { // Not a Message.
					// ...
				}
			}
		}
	})
}

func (client *Client) CmdHandler(msg *stdchat.CmdMsg) {
	switch msg.Command {
	case "raw":
		if client.svc.CheckArgs(2, msg) {
			values, err := url.ParseQuery(msg.Args[1])
			if err != nil {

			} else {
				client.enqueueRaw(msg.Args[0], values, msg.ID)
			}
		}
	default:
		client.tp.PublishError(msg.ID, msg.Network.ID,
			errors.New("unhandled command: "+msg.Command))
	}
}

func (client *Client) GetStateInfo() service.ClientStateInfo {
	msg := stdchat.NetworkStateInfo{}
	msg.Type = "network-state"
	msg.Ready = client.Ready()
	msg.Network.Init(client.NetworkID(), "net")
	msg.Network.SetName(client.NetworkName(), "")
	if msg.Ready {
		setUser(&msg.Myself, &client.bot.Self)
		client.setFeatures(&msg.Values)
	}
	msg.Protocol = Protocol
	client.mx.RLock()
	defer client.mx.RUnlock()
	var subs []stdchat.SubscriptionStateInfo
	for _, group := range client.groups {
		subs = append(subs, group.getStateInfoUnlocked(msg.Network))
	}
	return service.ClientStateInfo{Network: msg, Subscriptions: subs}
}

func (client *Client) enqueue(fn func()) error {
	select {
	case client.sendQueue <- fn:
		return nil
	case <-client.done:
		return errors.New("client closed")
	}
}

func (client *Client) sendLoop(sendQueue <-chan func()) {
	ctx := client.Context()
	for {
		select {
		case <-client.done:
			return
		case fn := <-sendQueue:
			err1 := client.lim1.Wait(ctx)
			err2 := client.lim2.Wait(ctx)
			if err1 != nil || err2 != nil {
				return
			}
			fn()
		}
	}
}

// Allows waiting for a sync request which is not an outgoing chat message.
// ctx should be client.Context() or derived from it.
func (client *Client) syncRequest(ctx context.Context) error {
	return client.lim2.Wait(ctx)
}

func NewClient(svc *service.Service, empty1, empty2, botToken string, values stdchat.ValuesInfo) (service.Networker, error) {
	if botToken == "" {
		return nil, errors.New("expected bot token")
	}
	sendQueue := make(chan func(), 32)
	client := &Client{
		sendQueue: sendQueue,
		svc:       svc,
		tp:        svc.Transporter(),
		bot:       nil,
		done:      make(chan struct{}),
		botToken:  botToken,
		// Adhere to limits per https://core.telegram.org/bots/faq
		// Waits on both limits.
		lim1:    rate.NewLimiter(1.0, 2),    // 1 per sec, burst 2.
		lim2:    rate.NewLimiter(0.333, 20), // 1 every 3 secs, burst 20.
		fileLim: rate.NewLimiter(1.0, 10),
	}
	go client.sendLoop(sendQueue)
	return client, nil
}
