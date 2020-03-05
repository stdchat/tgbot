// Copyright (C) 2020 Christopher E. Miller
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package tgbot

import "stdchat.org"

// Group is a tgbot group, channel, etc.
// TODO: isAdmin, isBot, ... and add them to member Values.
type Group struct {
	client     *Client
	Info       stdchat.EntityInfo
	members    []Member
	typ        string // group, supergroup, channel
	title      string // locked by mx
	photoURL   string // locked by mx, empty if none.
	photoThumb string // locked by mx, empty if none.
}

func (group *Group) GroupType() string {
	return group.typ
}

func (group *Group) GetTitle() string {
	group.client.mx.RLock()
	title := group.title
	group.client.mx.RUnlock()
	return title
}

func (group *Group) setTitle(title string) {
	group.client.mx.Lock()
	group.title = title
	group.client.mx.Unlock()
}

// GetPhoto returns photo URL and photo thumbnail URL.
// Both or thumb can be empty.
func (group *Group) GetPhoto() (string, string) {
	group.client.mx.RLock()
	a := group.photoURL
	b := group.photoThumb
	group.client.mx.RUnlock()
	return a, b
}

func (group *Group) setPhoto(url, thumb string) {
	group.client.mx.Lock()
	group.photoURL = url
	group.photoThumb = thumb
	group.client.mx.Unlock()
}

func (group *Group) migrateFrom(oldGroup *Group) {
	group.client.mx.Lock()
	defer group.client.mx.Lock()
	group.members = oldGroup.members
	oldGroup.members = nil
	group.title = oldGroup.title
	group.photoURL = oldGroup.photoURL
	group.photoThumb = oldGroup.photoThumb
}

// Returns true if added, false if already present.
func (group *Group) addMember(member Member) bool {
	group.client.mx.Lock()
	defer group.client.mx.Unlock()
	for _, xm := range group.members {
		if xm.User.ID == member.User.ID {
			return false
		}
	}
	group.members = append(group.members, member)
	return true
}

// use isInIDUnlocked to get index i.
func (group *Group) removeMemberUnlocked(i int) {
	ilast := len(group.members) - 1
	group.members[i], group.members[ilast] = group.members[ilast], Member{}
	group.members = group.members[:ilast]
}

func (group *Group) removeMember(id string) bool {
	group.client.mx.Lock()
	defer group.client.mx.Unlock()
	i := group.isInIDUnlocked(id)
	if i != -1 {
		group.removeMemberUnlocked(i)
		return true
	}
	return false
}

// Returns index in members, or -1
func (group *Group) isInIDUnlocked(id string) int {
	for i, member := range group.members {
		if member.User.ID == id {
			return i
		}
	}
	return -1
}

// Member.Valid() is false if user is not in this group.
func (group *Group) IsIn(id string) Member {
	group.client.mx.RLock()
	defer group.client.mx.RUnlock()
	i := group.isInIDUnlocked(id)
	if i == -1 {
		return Member{}
	}
	return group.members[i]
}

func (group *Group) getMembersInfoUnlocked() []stdchat.MemberInfo {
	members := make([]stdchat.MemberInfo, len(group.members))
	for i, member := range group.members {
		m := stdchat.MemberInfo{}
		m.Type = "member"
		m.Info.User = member.User
		members[i] = m
	}
	return members
}

func (group *Group) GetMembersInfo() []stdchat.MemberInfo {
	group.client.mx.RLock()
	defer group.client.mx.RUnlock()
	return group.getMembersInfoUnlocked()
}

func (group *Group) getStateInfoUnlocked(net stdchat.EntityInfo) stdchat.SubscriptionStateInfo {
	msg := stdchat.SubscriptionStateInfo{}
	msg.Type = "subscription-state"
	msg.Network = net
	msg.Protocol = Protocol
	msg.Destination = group.Info
	msg.Subject.SetText(group.title)
	msg.Members = group.getMembersInfoUnlocked()
	return msg
}

func (group *Group) GetStateInfo() stdchat.SubscriptionStateInfo {
	net := stdchat.EntityInfo{}
	net.Init(group.client.NetworkID(), "net")
	net.SetName(group.client.NetworkName(), "")
	group.client.mx.RLock()
	defer group.client.mx.RUnlock()
	return group.getStateInfoUnlocked(net)
}

type Member struct {
	User stdchat.EntityInfo
}

func (member Member) Valid() bool {
	return member.User.ID != ""
}
