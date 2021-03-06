package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amenzhinsky/golang-iothub/common"
	"github.com/amenzhinsky/golang-iothub/iotdevice/transport"
	"github.com/eclipse/paho.mqtt.golang"
)

const defaultQoS = 1

// TransportOption is a transport configuration option.
type TransportOption func(tr *Transport)

func WithLogger(l *log.Logger) TransportOption {
	return func(tr *Transport) {
		tr.logger = l
	}
}

// New returns new Transport transport.
// See more: https://docs.microsoft.com/en-us/azure/iot-hub/iot-hub-mqtt-support
func New(opts ...TransportOption) transport.Transport {
	tr := &Transport{done: make(chan struct{})}
	for _, opt := range opts {
		opt(tr)
	}
	return tr
}

type Transport struct {
	mu   sync.RWMutex
	conn mqtt.Client

	did string // device id
	rid uint32 // request id, incremented each request

	done chan struct{}         // closed when the transport is closed
	resp map[uint32]chan *resp // responses from iothub

	logger *log.Logger
}

type resp struct {
	code int
	body []byte

	ver int // twin response only
}

func (tr *Transport) logf(format string, v ...interface{}) {
	if tr.logger != nil {
		tr.logger.Printf(format, v...)
	}
}

func (tr *Transport) Connect(ctx context.Context, creds transport.Credentials) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.conn != nil {
		return errors.New("already connected")
	}

	o := mqtt.NewClientOptions()
	o.SetTLSConfig(creds.TLSConfig())

	if creds.IsSAS() {
		pwd, err := creds.Token(ctx, creds.Hostname(), time.Hour)
		if err != nil {
			return err
		}
		o.SetPassword(pwd)
	}

	o.AddBroker("tls://" + creds.Hostname() + ":8883")
	o.SetClientID(creds.DeviceID())
	o.SetUsername(creds.Hostname() + "/" + creds.DeviceID() + "/api-version=" + common.APIVersion)
	o.SetAutoReconnect(true)
	o.SetOnConnectHandler(func(_ mqtt.Client) {
		tr.logf("connection established")
	})
	o.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		tr.logf("connection lost: %v", err)
	})

	c := mqtt.NewClient(o)
	if err := contextToken(ctx, c.Connect()); err != nil {
		return err
	}

	tr.did = creds.DeviceID()
	tr.conn = c
	return nil
}

func (tr *Transport) SubscribeEvents(ctx context.Context, mux transport.MessageDispatcher) error {
	return contextToken(ctx, tr.conn.Subscribe(
		"devices/"+tr.did+"/messages/devicebound/#", defaultQoS, func(_ mqtt.Client, m mqtt.Message) {
			msg, err := parseEventMessage(m)
			if err != nil {
				tr.logf("parse error: %s", err)
				return
			}
			mux.Dispatch(msg)
		},
	))
}

func (tr *Transport) SubscribeTwinUpdates(ctx context.Context, mux transport.TwinStateDispatcher) error {
	return contextToken(ctx, tr.conn.Subscribe(
		"$iothub/twin/PATCH/properties/desired/#", defaultQoS, func(_ mqtt.Client, m mqtt.Message) {
			mux.Dispatch(m.Payload())
		},
	))
}

// mqtt library wraps errors with fmt.Errorf.
func (tr *Transport) IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Network Error")
}

func parseEventMessage(m mqtt.Message) (*common.Message, error) {
	p, err := parseCloudToDeviceTopic(m.Topic())
	if err != nil {
		return nil, err
	}
	e := &common.Message{
		Payload:    m.Payload(),
		Properties: make(map[string]string, len(p)),
	}
	for k, v := range p {
		switch k {
		case "$.mid":
			e.MessageID = v
		case "$.cid":
			e.CorrelationID = v
		case "$.uid":
			e.UserID = v
		case "$.to":
			e.To = v
		case "$.exp":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, err
			}
			e.ExpiryTime = &t
		default:
			e.Properties[k] = v
		}
	}
	return e, nil
}

// devices/{device}/messages/devicebound/%24.to=%2Fdevices%2F{device}%2Fmessages%2FdeviceBound&a=b&b=c
func parseCloudToDeviceTopic(s string) (map[string]string, error) {
	s, err := url.QueryUnescape(s)
	if err != nil {
		return nil, err
	}

	// attributes prefixed with $.,
	// e.g. `messageId` becomes `$.mid`, `to` becomes `$.to`, etc.
	i := strings.Index(s, "$.")
	if i == -1 {
		return nil, errors.New("malformed cloud-to-device topic name")
	}
	q, err := url.ParseQuery(s[i:])
	if err != nil {
		return nil, err
	}

	p := make(map[string]string, len(q))
	for k, v := range q {
		if len(v) != 1 {
			return nil, fmt.Errorf("unexpected number of property values: %d", len(q))
		}
		p[k] = v[0]
	}
	return p, nil
}

func (tr *Transport) RegisterDirectMethods(ctx context.Context, mux transport.MethodDispatcher) error {
	return contextToken(ctx, tr.conn.Subscribe(
		"$iothub/methods/POST/#", defaultQoS, func(_ mqtt.Client, m mqtt.Message) {
			method, rid, err := parseDirectMethodTopic(m.Topic())
			if err != nil {
				tr.logf("parse error: %s", err)
				return
			}
			rc, b, err := mux.Dispatch(method, m.Payload())
			if err != nil {
				tr.logf("dispatch error: %s", err)
				return
			}
			dst := fmt.Sprintf("$iothub/methods/res/%d/?$rid=%d", rc, rid)
			if err = tr.send(ctx, dst, defaultQoS, b); err != nil {
				tr.logf("method response error: %s", err)
				return
			}
		},
	))
}

// returns method name and rid
// format: $iothub/methods/POST/{method}/?$rid={rid}
func parseDirectMethodTopic(s string) (string, int, error) {
	const prefix = "$iothub/methods/POST/"

	s, err := url.QueryUnescape(s)
	if err != nil {
		return "", 0, err
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", 0, err
	}

	p := strings.TrimRight(u.Path, "/")
	if !strings.HasPrefix(p, prefix) {
		return "", 0, errors.New("malformed direct method topic")
	}

	q := u.Query()
	if len(q["$rid"]) != 1 {
		return "", 0, errors.New("$rid is not available")
	}
	rid, err := strconv.Atoi(q["$rid"][0])
	if err != nil {
		return "", 0, fmt.Errorf("$rid parse error: %s", err)
	}
	return p[len(prefix):], rid, nil
}

func (tr *Transport) RetrieveTwinProperties(ctx context.Context) ([]byte, error) {
	r, err := tr.request(ctx, "$iothub/twin/GET/?$rid=%d", nil)
	if err != nil {
		return nil, err
	}
	return r.body, nil
}

func (tr *Transport) UpdateTwinProperties(ctx context.Context, b []byte) (int, error) {
	r, err := tr.request(ctx, "$iothub/twin/PATCH/properties/reported/?$rid=%d", b)
	if err != nil {
		return 0, err
	}
	return r.ver, nil
}

func (tr *Transport) request(ctx context.Context, topic string, b []byte) (*resp, error) {
	if err := tr.enableTwinResponses(ctx); err != nil {
		return nil, err
	}
	rid := atomic.AddUint32(&tr.rid, 1) // increment rid counter
	dst := fmt.Sprintf(topic, rid)
	rch := make(chan *resp, 1)
	tr.mu.Lock()
	tr.resp[rid] = rch
	tr.mu.Unlock()
	defer func() {
		tr.mu.Lock()
		delete(tr.resp, rid)
		tr.mu.Unlock()
	}()

	if err := tr.send(ctx, dst, defaultQoS, b); err != nil {
		return nil, err
	}

	select {
	case r := <-rch:
		if r.code < 200 && r.code > 299 {
			return nil, fmt.Errorf("request failed with %d response code", r.code)
		}
		return r, nil
	case <-time.After(30 * time.Second):
		return nil, errors.New("request timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (tr *Transport) enableTwinResponses(ctx context.Context) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	// already subscribed
	if tr.resp != nil {
		return nil
	}

	if err := contextToken(ctx, tr.conn.Subscribe(
		"$iothub/twin/res/#", defaultQoS, func(_ mqtt.Client, m mqtt.Message) {
			rc, rid, ver, err := parseTwinPropsTopic(m.Topic())
			if err != nil {
				// TODO
				fmt.Printf("error: %s", err)
				return
			}

			tr.mu.RLock()
			defer tr.mu.RUnlock()
			for r, rch := range tr.resp {
				if int(r) != rid {
					continue
				}
				select {
				case rch <- &resp{code: rc, ver: ver, body: m.Payload()}:
				default:
					// we cannot allow blocking here,
					// buffered channel should solve it.
					panic("response sending blocked")
				}
				return
			}
			tr.logf("unknown rid: %q", rid)
		},
	)); err != nil {
		return err
	}

	tr.resp = make(map[uint32]chan *resp)
	return nil
}

// parseTwinPropsTopic parses the given topic name into rc, rid and ver.
// $iothub/twin/res/{rc}/?$rid={rid}(&$version={ver})?
func parseTwinPropsTopic(s string) (int, int, int, error) {
	const prefix = "$iothub/twin/res/"

	u, err := url.Parse(s)
	if err != nil {
		return 0, 0, 0, err
	}

	p := strings.Trim(u.Path, "/")
	if !strings.HasPrefix(p, prefix) {
		return 0, 0, 0, errors.New("malformed twin response topic")
	}
	rc, err := strconv.Atoi(p[len(prefix):])
	if err != nil {
		return 0, 0, 0, err
	}

	q := u.Query()
	if len(q["$rid"]) != 1 {
		return 0, 0, 0, errors.New("$rid is not available")
	}
	rid, err := strconv.Atoi(q["$rid"][0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("$rid parse error: %s", err)
	}

	var ver int // version is available only for update responses
	if len(q["$version"]) == 1 {
		ver, err = strconv.Atoi(q["$version"][0])
		if err != nil {
			return 0, 0, 0, err
		}
	}
	return rc, rid, ver, nil
}

func (tr *Transport) Send(ctx context.Context, msg *common.Message) error {
	// this is just copying functionality from the nodejs sdk, but
	// seems like adding meta attributes does nothing or in some cases,
	// e.g. when $.exp is set the cloud just disconnects.
	u := make(url.Values, len(msg.Properties)+5)
	if msg.MessageID != "" {
		u["$.mid"] = []string{msg.MessageID}
	}
	if msg.CorrelationID != "" {
		u["$.cid"] = []string{msg.CorrelationID}
	}
	if msg.UserID != "" {
		u["$.uid"] = []string{msg.UserID}
	}
	if msg.To != "" {
		u["$.to"] = []string{msg.To}
	}
	if msg.ExpiryTime != nil && !msg.ExpiryTime.IsZero() {
		u["$.exp"] = []string{msg.ExpiryTime.UTC().Format(time.RFC3339)}
	}
	for k, v := range msg.Properties {
		u[k] = []string{v}
	}

	dst := "devices/" + tr.did + "/messages/events/" + u.Encode()
	qos := defaultQoS
	if q, ok := msg.TransportOptions["qos"]; ok {
		qos = q.(int)
	}
	return tr.send(ctx, dst, qos, msg.Payload)
}

func (tr *Transport) send(ctx context.Context, topic string, qos int, b []byte) error {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	if tr.conn == nil {
		return errors.New("not connected")
	}
	return contextToken(ctx, tr.conn.Publish(topic, defaultQoS, false, b))
}

// mqtt lib doesn't support contexts currently
func contextToken(ctx context.Context, t mqtt.Token) error {
	done := make(chan struct{})
	go func() {
		for !t.WaitTimeout(time.Second) {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		close(done)
	}()
	select {
	case <-done:
		return t.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (tr *Transport) Close() error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	select {
	case <-tr.done:
		return nil
	default:
		close(tr.done)
	}
	if tr.conn != nil && tr.conn.IsConnected() {
		tr.conn.Disconnect(250)
		tr.logf("disconnected")
	}
	return nil
}
