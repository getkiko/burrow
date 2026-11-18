package main

import "github.com/getkiko/burrow/internal/signalproto"

func parseMessage(data []byte) (signalproto.Message, error) {
	return signalproto.Decode(data)
}

func encodeMessage(message signalproto.Message) []byte {
	data, _ := signalproto.Encode(message)
	return data
}

func encodeError(session, code, message string) []byte {
	return encodeMessage(signalproto.Message{
		Type:    signalproto.TypeError,
		Session: session,
		Code:    code,
		Error:   message,
	})
}
