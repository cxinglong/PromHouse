// promhouse
// Copyright (C) 2017 Percona LLC
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/Percona-Lab/PromHouse/prompb"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/model"
	"github.com/sirupsen/logrus"
)

// remote client for Prometheus remote APIs: read/write for PromHouse, read only for Prometheus.
type remoteClient struct {
	l    *logrus.Entry
	http *http.Client
	url  string

	start   time.Time
	end     time.Time
	step    time.Duration
	current time.Time

	bMarshaled, bEncoded []byte
	bRead, bDecoded      []byte
}

func newRemoteClient(url string, readStart, readEnd time.Time, readStep time.Duration) *remoteClient {
	return &remoteClient{
		l: logrus.WithField("client", "remote"),
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
			},
		},
		url: url,

		start:   readStart,
		end:     readEnd,
		step:    readStep,
		current: readStart,

		bMarshaled: make([]byte, 1048576),
		bEncoded:   make([]byte, 1048576),
		bRead:      make([]byte, 1048576),
		bDecoded:   make([]byte, 1048576),
	}
}

func (client *remoteClient) readTS() (*prompb.TimeSeries, error) {
	if client.current.Equal(client.end) {
		return nil, io.EOF
	}

	start := client.current
	end := start.Add(client.step)
	if end.After(client.end) {
		end = client.end
	}
	client.current = end

	request := prompb.ReadRequest{
		Queries: []*prompb.Query{{
			StartTimestampMs: int64(model.TimeFromUnixNano(start.UnixNano())),
			EndTimestampMs:   int64(model.TimeFromUnixNano(end.UnixNano())),
			Matchers: []*prompb.LabelMatcher{{
				Type:  prompb.LabelMatcher_RE,
				Name:  "__name__",
				Value: ".+",
			}},
		}},
	}
	client.l.Debugf("Request: %s", request)

	// marshal request reusing bMarshaled
	var err error
	size := request.Size()
	if cap(client.bMarshaled) >= size {
		client.bMarshaled = client.bMarshaled[:size]
	} else {
		client.bMarshaled = make([]byte, size)
	}
	size, err = request.MarshalTo(client.bMarshaled)
	if err != nil {
		return nil, err
	}
	if request.Size() != size {
		return nil, fmt.Errorf("unexpected size: expected %d, got %d", request.Size(), size)
	}

	// encode request reusing bEncoded
	client.bEncoded = client.bEncoded[:cap(client.bEncoded)]
	client.bEncoded = snappy.Encode(client.bEncoded, client.bMarshaled[:size])

	req, err := http.NewRequest("POST", client.url, bytes.NewReader(client.bEncoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")

	resp, err := client.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// read response reusing bRead
	buf := bytes.NewBuffer(client.bRead[:0])
	if _, err = buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	client.bRead = buf.Bytes()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, client.bRead)
	}

	// decode response reusing bDecoded
	client.bDecoded = client.bDecoded[:cap(client.bDecoded)]
	client.bDecoded, err = snappy.Decode(client.bDecoded, client.bRead)
	if err != nil {
		return nil, err
	}

	// unmarshal message
	var response prompb.ReadResponse
	if err = proto.Unmarshal(client.bDecoded, &response); err != nil {
		return nil, err
	}
	t := response.Results[0].TimeSeries
	l := len(t)
	switch l {
	case 0:
		client.l.Warnf("Got nothing for request %s.", request)
		return nil, nil
	case 1:
		client.l.Debugf("Got %s with %d samples.", t[0].Labels, len(t[0].Samples))
		return t[0], nil
	default:
		return nil, fmt.Errorf("expected 0 or 1 time series, got %d", l)
	}
}

func (client *remoteClient) writeTS(ts *prompb.TimeSeries) error {
	request := prompb.WriteRequest{
		TimeSeries: []*prompb.TimeSeries{ts},
	}
	client.l.Debugf("Request: %s", request)

	// marshal request reusing bMarshaled
	var err error
	size := request.Size()
	if cap(client.bMarshaled) >= size {
		client.bMarshaled = client.bMarshaled[:size]
	} else {
		client.bMarshaled = make([]byte, size)
	}
	size, err = request.MarshalTo(client.bMarshaled)
	if err != nil {
		return err
	}
	if request.Size() != size {
		return fmt.Errorf("unexpected size: expected %d, got %d", request.Size(), size)
	}

	// encode request reusing bEncoded
	client.bEncoded = client.bEncoded[:cap(client.bEncoded)]
	client.bEncoded = snappy.Encode(client.bEncoded, client.bMarshaled[:size])

	req, err := http.NewRequest("POST", client.url, bytes.NewReader(client.bEncoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")

	resp, err := client.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%d: %s", resp.StatusCode, b)
	}
	return nil
}

// check interfaces
var (
	_ tsReader = (*remoteClient)(nil)
	_ tsWriter = (*remoteClient)(nil)
)