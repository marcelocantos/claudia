// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia/internal/broker"
)

// judgeViaBroker runs one evaluation on the daemon: judge, then judged. One
// connection per evaluation, like a usage read. errNoBroker or
// errBrokerNotAvailable (brokerFellThrough) sends the caller to the direct
// path, and so does a daemon that predates Judge and answers unknown_type:
// such a daemon cannot evaluate the request, and the caller can.
func judgeViaBroker(ctx context.Context, body json.RawMessage) (*JudgeResult, error) {
	if len(body) > maxWireFrameBytes {
		// A state too large for one socket frame goes direct rather than
		// being refused by a transport the caller did not choose.
		return nil, errBrokerNotAvailable
	}
	client, err := dialBroker()
	if err != nil {
		return nil, err
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeJudge, Judge: &broker.JudgeRequest{Request: body}})
	if err != nil {
		var pe *broker.ProtocolError
		if errors.As(err, &pe) && pe.Code == broker.CodeUnknownType {
			return nil, fmt.Errorf("%w: daemon predates judge", errBrokerNotAvailable)
		}
		return nil, err
	}
	if resp.Judged == nil {
		return nil, fmt.Errorf("broker: judge answered with %s", resp.Type)
	}
	return decodeJudged(resp.Judged)
}

// decodeJudged turns the daemon's answer back into what the direct path
// would have returned: the result, ErrJudgeNoKey, or a *JudgeError with the
// API's status.
func decodeJudged(j *broker.JudgedResponse) (*JudgeResult, error) {
	if j.NoKey {
		return nil, ErrJudgeNoKey
	}
	if j.Error != "" {
		return nil, &JudgeError{Status: j.Status, Message: j.Error}
	}
	return DecodeJudgeResultWire(j.Result)
}

// EncodeJudged is the daemon's answer to a judge request, from what
// [RunJudgeWire] returned. An error that is neither ErrJudgeNoKey nor a
// *JudgeError (a transport failure between the daemon and the API) travels
// as a refusal with status 0.
func EncodeJudged(res *JudgeResult, err error) *broker.JudgedResponse {
	if err != nil {
		if errors.Is(err, ErrJudgeNoKey) {
			return &broker.JudgedResponse{NoKey: true, Error: err.Error()}
		}
		var je *JudgeError
		if errors.As(err, &je) {
			return &broker.JudgedResponse{Status: je.Status, Error: je.Message}
		}
		return &broker.JudgedResponse{Error: strings.TrimPrefix(err.Error(), judgeErrPrefix)}
	}
	raw, err := EncodeJudgeResultWire(res)
	if err != nil {
		return &broker.JudgedResponse{Error: "encode result: " + err.Error()}
	}
	return &broker.JudgedResponse{Result: raw}
}
