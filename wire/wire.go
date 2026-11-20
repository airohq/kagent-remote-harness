// Package wire preserves the original import path for the runner protocol.
package wire

import protocol "github.com/airohq/kagent-remote-harness/runner/wire"

const (
	Version         = protocol.Version
	ProtocolHeader  = protocol.ProtocolHeader
	InfoPath        = protocol.InfoPath
	TurnPath        = protocol.TurnPath
	ContentType     = protocol.ContentType
	MaxRequestBytes = protocol.MaxRequestBytes
	MaxEventBytes   = protocol.MaxEventBytes
	EventSession    = protocol.EventSession
	EventText       = protocol.EventText
	EventToolCall   = protocol.EventToolCall
	EventToolResult = protocol.EventToolResult
	EventFailure    = protocol.EventFailure
	EventDone       = protocol.EventDone
)

type Info = protocol.Info
type TurnRequest = protocol.TurnRequest
type Event = protocol.Event
