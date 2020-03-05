// Copyright (C) 2020 Christopher E. Miller
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package tgbot

import "stdchat.org/service"

func NewService(tp service.Transporter) *service.Service {
	return service.NewService(tp, NewClient)
}
