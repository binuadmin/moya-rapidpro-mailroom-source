package nexmo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nyaruka/gocommon/httpx"
	"github.com/nyaruka/gocommon/urns"
	"github.com/nyaruka/gocommon/uuids"
	"github.com/nyaruka/goflow/flows"
	"github.com/nyaruka/goflow/flows/events"
	"github.com/nyaruka/goflow/flows/routers/waits"
	"github.com/nyaruka/goflow/flows/routers/waits/hints"
	"github.com/nyaruka/goflow/utils"
	"github.com/nyaruka/mailroom/core/ivr"
	"github.com/nyaruka/mailroom/core/models"

	"github.com/buger/jsonparser"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gomodule/redigo/redis"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// CallURL is the API endpoint for Nexmo calls, public so our main IVR test can change it
var CallURL = `https://api.nexmo.com/v1/calls`

// IgnoreSignatures sets whether we ignore signatures (for unit tests)
var IgnoreSignatures = false

var callStatusMap = map[string]flows.DialStatus{
	"cancelled": flows.DialStatusFailed,
	"answered":  flows.DialStatusAnswered,
	"busy":      flows.DialStatusBusy,
	"timeout":   flows.DialStatusNoAnswer,
	"failed":    flows.DialStatusFailed,
	"rejected":  flows.DialStatusNoAnswer,
	"canceled":  flows.DialStatusFailed,
}

const (
	nexmoChannelType = models.ChannelType("NX")

	gatherTimeout = 30
	recordTimeout = 600

	appIDConfig      = "nexmo_app_id"
	privateKeyConfig = "nexmo_app_private_key"

	errorBody = `<?xml version="1.0" encoding="UTF-8"?>
	<Response>
		<Say>An error was encountered. Goodbye.</Say>
		<Hangup></Hangup>
	</Response>
	`

	statusFailed = "failed"
)

var indentMarshal = true

type client struct {
	httpClient *http.Client
	channel    *models.Channel
	callURL    string
	appID      string
	privateKey *rsa.PrivateKey
}

func init() {
	ivr.RegisterClientType(nexmoChannelType, NewClientFromChannel)
}

// NewClientFromChannel creates a new Twilio IVR client for the passed in account and and auth token
func NewClientFromChannel(httpClient *http.Client, channel *models.Channel) (ivr.Client, error) {
	appID := channel.ConfigValue(appIDConfig, "")
	key := channel.ConfigValue(privateKeyConfig, "")
	if appID == "" || key == "" {
		return nil, errors.Errorf("missing %s or %s on channel config", appIDConfig, privateKeyConfig)
	}

	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(key))
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing private key")
	}

	return &client{
		httpClient: httpClient,
		channel:    channel,
		callURL:    CallURL,
		appID:      appID,
		privateKey: privateKey,
	}, nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == http.NoBody {
		return nil, nil
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, nil
	}
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	return body, nil
}

func (c *client) CallIDForRequest(r *http.Request) (string, error) {
	body, err := readBody(r)
	if err != nil {
		return "", errors.Wrapf(err, "error reading body from request")
	}
	callID, err := jsonparser.GetString(body, "uuid")
	if err != nil {
		return "", errors.Errorf("invalid json body")
	}

	if callID == "" {
		return "", errors.Errorf("no uuid set on call")
	}
	return callID, nil
}

func (c *client) URNForRequest(r *http.Request) (urns.URN, error) {
	// get our recording url out
	body, err := readBody(r)
	if err != nil {
		return "", errors.Wrapf(err, "error reading body from request")
	}
	direction, _ := jsonparser.GetString(body, "direction")
	if direction == "" {
		direction = "inbound"
	}

	urnKey := ""
	switch direction {
	case "inbound":
		urnKey = "from"
	case "outbound":
		urnKey = "to"
	}

	urn, err := jsonparser.GetString(body, urnKey)
	if err != nil {
		return "", errors.Errorf("invalid json body")
	}

	if urn == "" {
		return "", errors.Errorf("no urn found in body")
	}
	return urns.NewTelURNForCountry("+"+urn, "")
}

func (c *client) DownloadMedia(url string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	token, err := c.generateToken()
	if err != nil {
		return nil, errors.Wrapf(err, "error generating jwt token")
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	return http.DefaultClient.Do(req)
}

func (c *client) PreprocessStatus(ctx context.Context, db *sqlx.DB, rp *redis.Pool, r *http.Request) ([]byte, error) {
	// parse out the call status, we are looking for a leg of one of our conferences ending in the "forward" case
	// get our recording url out
	body, _ := readBody(r)
	if len(body) == 0 {
		return nil, nil
	}

	// check the type of this status, we don't care about preprocessing "transfer" statuses
	nxType, err := jsonparser.GetString(body, "type")
	if err == jsonparser.MalformedJsonError {
		return nil, errors.Wrapf(err, "invalid json body: %s", body)
	}

	if nxType == "transfer" {
		return c.MakeEmptyResponseBody(fmt.Sprintf("ignoring conversation callback")), nil
	}

	// grab our uuid out
	legUUID, _ := jsonparser.GetString(body, "uuid")

	// and our status
	nxStatus, _ := jsonparser.GetString(body, "status")

	// if we are missing either, this is just a notification of the establishment of the conversation, ignore
	if legUUID == "" || nxStatus == "" {
		return nil, nil
	}

	// look up to see whether this is a call we need to track
	rc := rp.Get()
	defer rc.Close()

	redisKey := fmt.Sprintf("dial_%s", legUUID)
	dialContinue, err := redis.String(rc.Do("get", redisKey))

	logrus.WithField("redisKey", redisKey).WithField("redisValue", dialContinue).WithError(err).WithField("status", nxStatus).Debug("looking up dial continue")

	// no associated call, move on
	if err == redis.ErrNil {
		return nil, nil
	}

	if err != nil {
		return nil, errors.Wrapf(err, "error looking up leg uuid: %s", redisKey)
	}

	// transfer the call back to our handle with the dial wait type
	parts := strings.SplitN(dialContinue, ":", 2)
	callUUID, resumeURL := parts[0], parts[1]

	// we found an associated call, if the status is complete, have it continue, we call out to
	// redis and hand it our flow to resume on to get the next NCCO
	if nxStatus == "completed" {
		logrus.Debug("found completed call, trying to finish with call ID: ", callUUID)
		statusKey := fmt.Sprintf("dial_status_%s", callUUID)
		status, err := redis.String(rc.Do("get", statusKey))
		if err == redis.ErrNil {
			return nil, fmt.Errorf("unable to find call status for: %s", callUUID)
		}
		if err != nil {
			return nil, errors.Wrapf(err, "error looking up call status for: %s", callUUID)
		}

		// duration of the call is in our body
		duration, _ := jsonparser.GetString(body, "duration")

		resumeURL += "&dial_status=" + status
		resumeURL += "&dial_duration=" + duration
		resumeURL += "&sig=" + c.calculateSignature(resumeURL)

		nxBody := map[string]interface{}{
			"action": "transfer",
			"destination": map[string]interface{}{
				"type": "ncco",
				"url":  []string{resumeURL},
			},
		}
		trace, err := c.makeRequest(http.MethodPut, c.callURL+"/"+callUUID, nxBody)
		if err != nil {
			return nil, errors.Wrapf(err, "error reconnecting flow for call: %s", callUUID)
		}

		// nexmo return 204 on successful updates
		if trace.Response.StatusCode != http.StatusNoContent {
			return nil, fmt.Errorf("error reconnecting flow for call: %s, received %d from nexmo", callUUID, trace.Response.StatusCode)
		}

		return c.MakeEmptyResponseBody(fmt.Sprintf("reconnected call: %s to flow with dial status: %s", callUUID, status)), nil
	}

	// otherwise the call isn't over yet, instead stash away our status so we can use it to
	// determine if the call was answered, busy etc..
	status := callStatusMap[nxStatus]

	// only store away valid final states
	if status != "" {
		redisKey := fmt.Sprintf("dial_status_%s", callUUID)
		_, err = rc.Do("setex", redisKey, 300, status)
		if err != nil {
			return nil, errors.Wrapf(err, "error inserting recording URL into redis")
		}

		logrus.WithField("redisKey", redisKey).WithField("status", status).WithField("callUUID", callUUID).Debug("saved intermediary dial status for call")
		return c.MakeEmptyResponseBody(fmt.Sprintf("updated status for call: %s to: %s", callUUID, status)), nil
	}

	return c.MakeEmptyResponseBody(fmt.Sprintf("ignoring non final status for tranfer leg")), nil
}

func (c *client) PreprocessResume(ctx context.Context, db *sqlx.DB, rp *redis.Pool, conn *models.ChannelConnection, r *http.Request) ([]byte, error) {
	// if this is a recording_url resume, grab that
	waitType := r.URL.Query().Get("wait_type")

	switch waitType {
	case "record":
		recordingUUID := r.URL.Query().Get("recording_uuid")
		if recordingUUID == "" {
			return nil, errors.Errorf("record resume without recording_uuid")
		}

		rc := rp.Get()
		defer rc.Close()

		redisKey := fmt.Sprintf("recording_%s", recordingUUID)
		recordingURL, err := redis.String(rc.Do("get", redisKey))
		if err != nil && err != redis.ErrNil {
			return nil, errors.Wrapf(err, "error getting recording url from redis")
		}

		// found a URL, stuff it in our request and move on
		if recordingURL != "" {
			r.URL.RawQuery = "&recording_url=" + url.QueryEscape(recordingURL)
			logrus.WithField("recording_url", recordingURL).Info("found recording URL")
			rc.Do("del", redisKey)
			return nil, nil
		}

		// no recording yet, send back another 1 second input / wait
		path := r.URL.RequestURI()
		proxyPath := r.Header.Get("X-Forwarded-Path")
		if proxyPath != "" {
			path = proxyPath
		}
		url := fmt.Sprintf("https://%s%s", r.Host, path)

		input := &Input{
			Action:       "input",
			Timeout:      1,
			SubmitOnHash: true,
			EventURL:     []string{url},
			EventMethod:  http.MethodPost,
		}
		return json.MarshalIndent([]interface{}{input}, "", "  ")

	case "recording_url":
		// this is our async callback for our recording URL, we stuff it in redis and return an empty response
		recordingUUID := r.URL.Query().Get("recording_uuid")
		if recordingUUID == "" {
			return nil, errors.Errorf("recording_url resume without recording_uuid")
		}

		// get our recording url out
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, errors.Wrapf(err, "error reading body from request")
		}
		recordingURL, err := jsonparser.GetString(body, "recording_url")
		if err != nil {
			return nil, errors.Errorf("invalid json body")
		}
		if recordingURL == "" {
			return nil, errors.Errorf("no recording_url found in request")
		}

		// write it to redis
		rc := rp.Get()
		defer rc.Close()

		redisKey := fmt.Sprintf("recording_%s", recordingUUID)
		_, err = rc.Do("setex", redisKey, 300, recordingURL)
		if err != nil {
			return nil, errors.Wrapf(err, "error inserting recording URL into redis")
		}

		msgBody := map[string]string{
			"_message": fmt.Sprintf("inserted recording url: %s for uuid: %s", recordingURL, recordingUUID),
		}
		return json.MarshalIndent(msgBody, "", "  ")

	default:
		return nil, nil
	}
}

type Phone struct {
	Type   string `json:"type"`
	Number string `json:"number"`
}

type NCCO struct {
	Action string `json:"action"`
	Name   string `json:"name"`
}

type CallRequest struct {
	To           []Phone  `json:"to"`
	From         Phone    `json:"from"`
	AnswerURL    []string `json:"answer_url"`
	AnswerMethod string   `json:"answer_method"`
	EventURL     []string `json:"event_url"`
	EventMethod  string   `json:"event_method"`

	NCCO         []NCCO `json:"ncco,omitempty"`
	RingingTimer int    `json:"ringing_timer,omitempty"`
}

// CallResponse is our struct for a Nexmo call response
// {
//  "uuid": "63f61863-4a51-4f6b-86e1-46edebcf9356",
//  "status": "started",
//  "direction": "outbound",
//  "conversation_uuid": "CON-f972836a-550f-45fa-956c-12a2ab5b7d22"
// }
type CallResponse struct {
	UUID             string `json:"uuid"`
	Status           string `json:"status"`
	Direction        string `json:"direction"`
	ConversationUUID string `json:"conversation_uuid"`
}

// RequestCall causes this client to request a new outgoing call for this provider
func (c *client) RequestCall(number urns.URN, resumeURL string, statusURL string) (ivr.CallID, *httpx.Trace, error) {
	callR := &CallRequest{
		AnswerURL:    []string{resumeURL + "&sig=" + url.QueryEscape(c.calculateSignature(resumeURL))},
		AnswerMethod: http.MethodPost,

		EventURL:    []string{statusURL + "?sig=" + url.QueryEscape(c.calculateSignature(statusURL))},
		EventMethod: http.MethodPost,
	}

	callR.To = append(callR.To, Phone{Type: "phone", Number: strings.TrimLeft(number.Path(), "+")})
	callR.From = Phone{Type: "phone", Number: strings.TrimLeft(c.channel.Address(), "+")}

	trace, err := c.makeRequest(http.MethodPost, c.callURL, callR)
	if err != nil {
		return ivr.NilCallID, trace, errors.Wrapf(err, "error trying to start call")
	}

	if trace.Response.StatusCode != http.StatusCreated {
		return ivr.NilCallID, trace, errors.Errorf("received non 200 status for call start: %d", trace.Response.StatusCode)
	}

	// parse out our call sid
	call := &CallResponse{}
	err = json.Unmarshal(trace.ResponseBody, call)
	if err != nil || call.UUID == "" {
		return ivr.NilCallID, trace, errors.Errorf("unable to read call uuid")
	}

	if call.Status == statusFailed {
		return ivr.NilCallID, trace, errors.Errorf("call status returned as failed")
	}

	logrus.WithField("body", string(trace.ResponseBody)).WithField("status", trace.Response.StatusCode).Debug("requested call")

	return ivr.CallID(call.UUID), trace, nil
}

// HangupCall asks Nexmo to hang up the call that is passed in
func (c *client) HangupCall(callID string) (*httpx.Trace, error) {
	hangupBody := map[string]string{"action": "hangup"}
	url := c.callURL + "/" + callID
	trace, err := c.makeRequest(http.MethodPut, url, hangupBody)
	if err != nil {
		return trace, errors.Wrapf(err, "error trying to hangup call")
	}

	if trace.Response.StatusCode != 204 {
		return trace, errors.Errorf("received non 204 status for call hangup: %d", trace.Response.StatusCode)
	}
	return trace, nil
}

type NCCOInput struct {
	DTMF             string `json:"dtmf"`
	TimedOut         bool   `json:"timed_out"`
	UUID             string `json:"uuid"`
	ConversationUUID string `json:"conversation_uuid"`
	Timestamp        string `json:"timestamp"`
}

// ResumeForRequest returns the resume (input or dial) for the passed in request, if any
func (c *client) ResumeForRequest(r *http.Request) (ivr.Resume, error) {
	// this could be empty, in which case we return nothing at all
	empty := r.Form.Get("empty")
	if empty == "true" {
		return ivr.InputResume{}, nil
	}

	waitType := r.Form.Get("wait_type")

	// if this is an input, parse that
	if waitType == "gather" || waitType == "record" {
		// parse our input
		input := &NCCOInput{}
		bb, err := readBody(r)
		if err != nil {
			return nil, errors.Wrapf(err, "error reading request body")
		}

		err = json.Unmarshal(bb, input)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to parse ncco request")
		}

		// otherwise grab the right field based on our wait type
		switch waitType {
		case "gather":
			// this could be a timeout, in which case we return nothing at all
			if input.TimedOut {
				return ivr.InputResume{}, nil
			}

			return ivr.InputResume{Input: input.DTMF}, nil

		case "record":
			recordingURL := r.URL.Query().Get("recording_url")
			if recordingURL == "" {
				return ivr.InputResume{}, nil
			}
			logrus.WithField("recording_url", recordingURL).Info("input found recording")
			return ivr.InputResume{Attachment: utils.Attachment("audio:" + recordingURL)}, nil

		default:
			return nil, errors.Errorf("unknown wait_type: %s", waitType)
		}
	}

	// non-input case of getting a dial
	switch waitType {
	case "dial":
		status := r.URL.Query().Get("dial_status")
		if status == "" {
			return nil, errors.Errorf("unable to find dial_status in query url")
		}
		duration := 0
		d := r.URL.Query().Get("dial_duration")
		if d != "" {
			parsed, err := strconv.Atoi(d)
			if err != nil {
				return nil, errors.Errorf("non-integer duration in query url")
			}
			duration = parsed
		}

		logrus.WithField("status", status).WithField("duration", duration).Info("input found dial status and duration")
		return ivr.DialResume{Status: flows.DialStatus(status), Duration: duration}, nil

	default:
		return nil, errors.Errorf("unknown wait_type: %s", waitType)
	}
}

type StatusRequest struct {
	UUID     string `json:"uuid"`
	Status   string `json:"status"`
	Duration string `json:"duration"`
}

// StatusForRequest returns the current call status for the passed in status (and optional duration if known)
func (c *client) StatusForRequest(r *http.Request) (models.ConnectionStatus, int) {
	// this is a resume, call is in progress, no need to look at the body
	if r.Form.Get("action") == "resume" {
		return models.ConnectionStatusInProgress, 0
	}

	status := &StatusRequest{}
	bb, err := readBody(r)
	if err != nil {
		logrus.WithError(err).Error("error reading status request body")
		return models.ConnectionStatusErrored, 0
	}
	err = json.Unmarshal(bb, status)
	if err != nil {
		logrus.WithError(err).WithField("body", string(bb)).Error("error unmarshalling ncco status")
		return models.ConnectionStatusErrored, 0
	}

	// transfer status callbacks have no status, safe to ignore them
	if status.Status == "" {
		return models.ConnectionStatusInProgress, 0
	}

	switch status.Status {

	case "started", "ringing":
		return models.ConnectionStatusWired, 0

	case "answered":
		return models.ConnectionStatusInProgress, 0

	case "completed":
		duration, _ := strconv.Atoi(status.Duration)
		return models.ConnectionStatusCompleted, duration

	case "rejected", "busy", "unanswered", "timeout", "failed", "machine":
		return models.ConnectionStatusErrored, 0

	default:
		logrus.WithField("status", status.Status).Error("unknown call status in ncco callback")
		return models.ConnectionStatusFailed, 0
	}
}

// ValidateRequestSignature validates the signature on the passed in request, returning an error if it is invaled
func (c *client) ValidateRequestSignature(r *http.Request) error {
	if IgnoreSignatures {
		return nil
	}

	// only validate handling calls, we can't verify others
	if !strings.HasSuffix(r.URL.Path, "handle") {
		return nil
	}

	actual := r.URL.Query().Get("sig")
	if actual == "" {
		return errors.Errorf("missing request sig")
	}

	path := r.URL.RequestURI()
	proxyPath := r.Header.Get("X-Forwarded-Path")
	if proxyPath != "" {
		path = proxyPath
	}

	url := fmt.Sprintf("https://%s%s", r.Host, path)
	expected := c.calculateSignature(url)
	if expected != actual {
		return errors.Errorf("mismatch in signatures for url: %s, actual: %s, expected: %s", url, actual, expected)
	}
	return nil
}

// WriteSessionResponse writes a NCCO response for the events in the passed in session
func (c *client) WriteSessionResponse(ctx context.Context, rp *redis.Pool, channel *models.Channel, conn *models.ChannelConnection, session *models.Session, number urns.URN, resumeURL string, r *http.Request, w http.ResponseWriter) error {
	// for errored sessions we should just output our error body
	if session.Status() == models.SessionStatusFailed {
		return errors.Errorf("cannot write IVR response for failed session")
	}

	// otherwise look for any say events
	sprint := session.Sprint()
	if sprint == nil {
		return errors.Errorf("cannot write IVR response for session with no sprint")
	}

	// get our response
	response, err := c.responseForSprint(ctx, rp, channel, conn, resumeURL, session.Wait(), sprint.Events())
	if err != nil {
		return errors.Wrap(err, "unable to build response for IVR call")
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write([]byte(response))
	if err != nil {
		return errors.Wrap(err, "error writing IVR response")
	}

	return nil
}

// WriteErrorResponse writes an error / unavailable response
func (c *client) WriteErrorResponse(w http.ResponseWriter, err error) error {
	actions := []interface{}{Talk{
		Action: "talk",
		Text:   ivr.ErrorMessage,
		Error:  err.Error(),
	}}
	body, err := json.Marshal(actions)
	if err != nil {
		return errors.Wrapf(err, "error marshalling ncco error")
	}

	_, err = w.Write(body)
	return err
}

// WriteEmptyResponse writes an empty (but valid) response
func (c *client) WriteEmptyResponse(w http.ResponseWriter, msg string) error {
	_, err := w.Write(c.MakeEmptyResponseBody(msg))
	return err
}

func (c *client) MakeEmptyResponseBody(msg string) []byte {
	msgBody := map[string]string{
		"_message": msg,
	}
	body, err := json.Marshal(msgBody)
	if err != nil {
		panic(errors.Wrapf(err, "error marshalling message"))
	}
	return body
}

func (c *client) makeRequest(method string, sendURL string, body interface{}) (*httpx.Trace, error) {
	bb, err := json.Marshal(body)
	if err != nil {
		return nil, errors.Wrapf(err, "error json encoding request")
	}

	req, _ := http.NewRequest(method, sendURL, bytes.NewReader(bb))
	token, err := c.generateToken()
	if err != nil {
		return nil, errors.Wrapf(err, "error generating jwt token")
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	return httpx.DoTrace(c.httpClient, req, nil, nil, -1)
}

// calculateSignature calculates a signature for the passed in URL
func (c *client) calculateSignature(u string) string {
	url, _ := url.Parse(u)

	var buffer bytes.Buffer
	buffer.WriteString(url.Scheme)
	buffer.WriteString("://")
	buffer.WriteString(url.Host)
	buffer.WriteString(url.Path)

	form := url.Query()
	keys := make(sort.StringSlice, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	keys.Sort()

	for _, k := range keys {
		// ignore sig parameter
		if k == "sig" {
			continue
		}

		buffer.WriteString(k)
		for _, v := range form[k] {
			buffer.WriteString(v)
		}
	}

	// hash with SHA1
	mac := hmac.New(sha1.New, []byte(c.appID))
	mac.Write(buffer.Bytes())
	hash := mac.Sum(nil)

	// encode with Base64
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(hash)))
	base64.StdEncoding.Encode(encoded, hash)

	return string(encoded)
}

type jwtClaims struct {
	ApplicationID string `json:"application_id"`
	jwt.StandardClaims
}

func (c *client) generateToken() (string, error) {
	claims := jwtClaims{
		c.appID,
		jwt.StandardClaims{
			Id:       strconv.Itoa(rand.Int()),
			IssuedAt: time.Now().UTC().Unix(),
		},
	}
	token := jwt.NewWithClaims(jwt.GetSigningMethod("RS256"), claims)
	return token.SignedString(c.privateKey)
}

// NCCO building utilities

type Talk struct {
	Action  string `json:"action"`
	Text    string `json:"text"`
	BargeIn bool   `json:"bargeIn,omitempty"`
	Error   string `json:"_error,omitempty"`
	Message string `json:"_message,omitempty"`
}

type Stream struct {
	Action    string   `json:"action"`
	StreamURL []string `json:"streamUrl"`
}

type Hangup struct {
	XMLName string `xml:"Hangup"`
}

type Redirect struct {
	XMLName string `xml:"Redirect"`
	URL     string `xml:",chardata"`
}

type Input struct {
	Action       string   `json:"action"`
	MaxDigits    int      `json:"maxDigits,omitempty"`
	SubmitOnHash bool     `json:"submitOnHash"`
	Timeout      int      `json:"timeOut"`
	EventURL     []string `json:"eventUrl"`
	EventMethod  string   `json:"eventMethod"`
}

type Record struct {
	Action       string   `json:"action"`
	EndOnKey     string   `json:"endOnKey,omitempty"`
	Timeout      int      `json:"timeOut,omitempty"`
	EndOnSilence int      `json:"endOnSilence,omitempty"`
	EventURL     []string `json:"eventUrl"`
	EventMethod  string   `json:"eventMethod"`
}

type Endpoint struct {
	Type   string `json:"type"`
	Number string `json:"number"`
}

type Conversation struct {
	Action string `json:"action"`
	Name   string `json:"name"`
}

func (c *client) responseForSprint(ctx context.Context, rp *redis.Pool, channel *models.Channel, conn *models.ChannelConnection, resumeURL string, w flows.ActivatedWait, es []flows.Event) (string, error) {
	actions := make([]interface{}, 0, 1)
	waitActions := make([]interface{}, 0, 1)

	if w != nil {
		switch wait := w.(type) {
		case *waits.ActivatedMsgWait:
			switch hint := wait.Hint().(type) {
			case *hints.DigitsHint:
				eventURL := resumeURL + "&wait_type=gather"
				eventURL = eventURL + "&sig=" + url.QueryEscape(c.calculateSignature(eventURL))
				input := &Input{
					Action:       "input",
					Timeout:      gatherTimeout,
					SubmitOnHash: true,
					EventURL:     []string{eventURL},
					EventMethod:  http.MethodPost,
				}
				// limit our digits if asked to
				if hint.Count != nil {
					input.MaxDigits = *hint.Count
				} else {
					input.MaxDigits = 20
				}
				waitActions = append(waitActions, input)

			case *hints.AudioHint:
				// Nexmo is goofy in that they do not synchronously send us recordings. Rather the move on in
				// the NCCO script immediately and then asynchronously call the event URL on the record URL
				// when the recording is ready.
				//
				// We deal with this by adding the record event with a status callback including a UUID
				// which we will store the recording url under when it is received. Meanwhile we put an input
				// with a 1 second timeout in the script that will get called / repeated until the UUID is
				// populated at which time we will actually continue.

				recordingUUID := string(uuids.New())
				eventURL := resumeURL + "&wait_type=recording_url&recording_uuid=" + recordingUUID
				eventURL = eventURL + "&sig=" + url.QueryEscape(c.calculateSignature(eventURL))
				record := &Record{
					Action:       "record",
					EventURL:     []string{eventURL},
					EventMethod:  http.MethodPost,
					EndOnKey:     "#",
					Timeout:      recordTimeout,
					EndOnSilence: 5,
				}
				waitActions = append(waitActions, record)

				// nexmo is goofy in that they do not call our event URL upon gathering the recording but
				// instead move on. So we need to put in an input here as well
				eventURL = resumeURL + "&wait_type=record&recording_uuid=" + recordingUUID
				eventURL = eventURL + "&sig=" + url.QueryEscape(c.calculateSignature(eventURL))
				input := &Input{
					Action:       "input",
					Timeout:      1,
					SubmitOnHash: true,
					EventURL:     []string{eventURL},
					EventMethod:  http.MethodPost,
				}
				waitActions = append(waitActions, input)

			default:
				return "", errors.Errorf("unable to use wait in IVR call, unknow hint type: %s", wait.Hint().Type())
			}

		case *waits.ActivatedDialWait:
			// Nexmo handles forwards a bit differently. We have to create a new call to the forwarded number, then
			// join the current call with the call we are starting.
			//
			// See: https://developer.nexmo.com/use-cases/contact-center
			//
			// We then track the state of that call, restarting NCCO control of the original call when
			// the transfer has completed.
			conversationUUID := string(uuids.New())
			connect := &Conversation{
				Action: "conversation",
				Name:   conversationUUID,
			}
			waitActions = append(waitActions, connect)

			// create our outbound call with the same conversation UUID
			call := CallRequest{}
			call.To = append(call.To, Phone{Type: "phone", Number: strings.TrimLeft(wait.URN().Path(), "+")})
			call.From = Phone{Type: "phone", Number: strings.TrimLeft(channel.Address(), "+")}
			call.NCCO = append(call.NCCO, NCCO{Action: "conversation", Name: conversationUUID})
			if wait.TimeoutSeconds() != nil {
				call.RingingTimer = *wait.TimeoutSeconds()
			}

			trace, err := c.makeRequest(http.MethodPost, c.callURL, call)
			logrus.WithField("trace", trace).Debug("initiated new call for transfer")
			if err != nil {
				return "", errors.Wrapf(err, "error trying to start call")
			}

			if trace.Response.StatusCode != http.StatusCreated {
				return "", errors.Errorf("received non 200 status for call start: %d", trace.Response.StatusCode)
			}

			// we save away our call id, as we want to continue our original call when that is complete
			transferUUID, err := jsonparser.GetString(trace.ResponseBody, "uuid")
			if err != nil {
				return "", errors.Wrapf(err, "error reading call id from transfer")
			}

			// save away the tranfer id, connecting it to this connection
			rc := rp.Get()
			defer rc.Close()

			eventURL := resumeURL + "&wait_type=dial"
			redisKey := fmt.Sprintf("dial_%s", transferUUID)
			redisValue := fmt.Sprintf("%s:%s", conn.ExternalID(), eventURL)
			_, err = rc.Do("setex", redisKey, 3600, redisValue)
			if err != nil {
				return "", errors.Wrapf(err, "error inserting transfer ID into redis")
			}
			logrus.WithField("transferUUID", transferUUID).WithField("callID", conn.ExternalID()).WithField("redisKey", redisKey).WithField("redisValue", redisValue).Debug("saved away call id")

		default:
			return "", errors.Errorf("unable to use wait in IVR call, unknow wait type: %s", w)
		}
	}

	isWaitInput := false
	if len(waitActions) > 0 {
		_, isWaitInput = waitActions[0].(*Input)
	}

	for _, e := range es {
		switch event := e.(type) {
		case *events.IVRCreatedEvent:
			if len(event.Msg.Attachments()) == 0 {
				actions = append(actions, Talk{
					Action:  "talk",
					Text:    event.Msg.Text(),
					BargeIn: isWaitInput,
				})
			} else {
				for _, a := range event.Msg.Attachments() {
					actions = append(actions, Stream{
						Action:    "stream",
						StreamURL: []string{a.URL()},
					})
				}
			}
		}
	}

	for _, w := range waitActions {
		actions = append(actions, w)
	}

	var body []byte
	var err error
	if indentMarshal {
		body, err = json.MarshalIndent(actions, "", "  ")
	} else {
		body, err = json.Marshal(actions)
	}
	if err != nil {
		return "", errors.Wrap(err, "unable to marshal ncco body")
	}

	return string(body), nil
}
