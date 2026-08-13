package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

func apiAMQPFrame(typ byte, channel uint16, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := []byte{typ}
	out = binary.BigEndian.AppendUint16(out, channel)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, body...)
	return append(out, 0xCE)
}

func apiAMQPMethod(channel, class, method uint16, args ...[]byte) []byte {
	return apiAMQPFrame(1, channel,
		binary.BigEndian.AppendUint16(nil, class),
		binary.BigEndian.AppendUint16(nil, method),
		bytes.Join(args, nil),
	)
}

func apiAMQPShort(value string) []byte { return append([]byte{byte(len(value))}, value...) }

func TestAnAMQPCaptureIsReadBackAsFrames(t *testing.T) {
	body := []byte(`{"order_id":417}`)
	stream := bytes.Join([][]byte{
		apiAMQPMethod(1, 60, 40, []byte{0, 0}, apiAMQPShort("orders"), apiAMQPShort("created"), []byte{0}),
		apiAMQPFrame(2, 1,
			binary.BigEndian.AppendUint16(nil, 60), []byte{0, 0},
			binary.BigEndian.AppendUint64(nil, uint64(len(body))), []byte{0, 0}),
		apiAMQPFrame(3, 1, body),
	}, nil)

	view := buildAMQPView(&store.Call{Protocol: config.ProtocolAMQP, Request: store.Message{Body: stream}})
	if view.SentIncomplete || len(view.Sent) != 3 {
		t.Fatalf("sent = %+v incomplete=%v", view.Sent, view.SentIncomplete)
	}
	if view.Sent[0].Kind != "basic.publish" || view.Sent[0].Exchange != "orders" || view.Sent[2].Text != string(body) {
		t.Errorf("decoded frames = %+v", view.Sent)
	}
}

func TestTheCallAPISerialisesAMQPWithoutRawFramePayloads(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	frame := apiAMQPMethod(0, 10, 11,
		[]byte{0, 0, 0, 0}, apiAMQPShort("PLAIN"),
		[]byte{0, 0, 0, 6, 0, 0, 0, 0, 0, 0}, apiAMQPShort("en_US"),
	)
	id, err := db.Insert(context.Background(), &store.Call{
		Target: "rabbit", Protocol: config.ProtocolAMQP, Method: "connection.start-ok", Path: "connection",
		ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Request: store.Message{Body: frame, Size: int64(len(frame))},
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := New(db, noDrops{}, emptyRuntime(t, db)).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/calls/"+strconv.FormatInt(id, 10), nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{`"protocol":"amqp"`, `"amqp":`, `"kind":"connection.start-ok"`, `"mechanism":"PLAIN"`} {
		if !strings.Contains(out, want) {
			t.Errorf("API response is missing %s: %s", want, out)
		}
	}
}
