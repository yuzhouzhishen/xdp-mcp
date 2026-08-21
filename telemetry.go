package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	paho "github.com/eclipse/paho.golang/paho"
	"github.com/golang/glog"
	"google.golang.org/protobuf/proto"

	pb "xdp-mcp/proto/xdp/v1"
)

const telemetryStaleness = 30 * time.Second

// TelemetryManager maintains a live telemetry stream per PSN.
// It owns the MQTT subscriptions for telemetry response topics
// and caches the latest port and device snapshots.
type TelemetryManager struct {
	mqtt    *MQTTClient
	mu      sync.Mutex
	streams map[uint64]*telemetryStream
}

type telemetryStream struct {
	cancel        context.CancelFunc
	latestPort    *pb.StreamPortStatus
	updatedPort   time.Time
	latestDevice  *pb.StreamDeviceStatus
	updatedDevice time.Time
	latestMu      sync.RWMutex
}

func NewTelemetryManager(mqtt *MQTTClient) *TelemetryManager {
	return &TelemetryManager{
		mqtt:    mqtt,
		streams: make(map[uint64]*telemetryStream),
	}
}

// Start begins a telemetry stream for the given PSN. Idempotent.
func (tm *TelemetryManager) Start(ctx context.Context, psn uint64) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, exists := tm.streams[psn]; exists {
		return
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	ts := &telemetryStream{cancel: cancel}
	tm.streams[psn] = ts

	go tm.run(streamCtx, psn, ts)
	glog.Infof("telemetry: started psn=%d", psn)
}

// Stop tears down the telemetry stream for a PSN. Idempotent.
func (tm *TelemetryManager) Stop(ctx context.Context, psn uint64) {
	tm.mu.Lock()
	ts, exists := tm.streams[psn]
	if !exists {
		tm.mu.Unlock()
		return
	}
	delete(tm.streams, psn)
	tm.mu.Unlock()

	ts.cancel()
	glog.Infof("telemetry: stopped psn=%d", psn)
}

// Latest returns the cached StreamPortStatus and its timestamp, or (nil, zero).
func (tm *TelemetryManager) Latest(psn uint64) (*pb.StreamPortStatus, time.Time) {
	tm.mu.Lock()
	ts, exists := tm.streams[psn]
	tm.mu.Unlock()
	if !exists {
		return nil, time.Time{}
	}
	ts.latestMu.RLock()
	defer ts.latestMu.RUnlock()
	return ts.latestPort, ts.updatedPort
}

// LatestDevice returns the cached StreamDeviceStatus and its timestamp, or (nil, zero).
func (tm *TelemetryManager) LatestDevice(psn uint64) (*pb.StreamDeviceStatus, time.Time) {
	tm.mu.Lock()
	ts, exists := tm.streams[psn]
	tm.mu.Unlock()
	if !exists {
		return nil, time.Time{}
	}
	ts.latestMu.RLock()
	defer ts.latestMu.RUnlock()
	return ts.latestDevice, ts.updatedDevice
}

// Kick re-sends the start command if data is stale or absent.
// If no stream exists yet, starts one.
func (tm *TelemetryManager) Kick(ctx context.Context, psn uint64) {
	tm.mu.Lock()
	ts, exists := tm.streams[psn]
	tm.mu.Unlock()

	if !exists {
		tm.Start(ctx, psn)
		return
	}

	ts.latestMu.RLock()
	stale := ts.latestPort == nil || time.Since(ts.updatedPort) > telemetryStaleness
	ts.latestMu.RUnlock()

	if stale {
		tm.kick(ctx, psn)
	}
}

// KickDevice re-sends the start command if device telemetry is stale or absent.
// If no stream exists yet, starts one.
func (tm *TelemetryManager) KickDevice(ctx context.Context, psn uint64) {
	tm.mu.Lock()
	ts, exists := tm.streams[psn]
	tm.mu.Unlock()

	if !exists {
		tm.Start(ctx, psn)
		return
	}

	ts.latestMu.RLock()
	stale := ts.latestDevice == nil || time.Since(ts.updatedDevice) > telemetryStaleness
	ts.latestMu.RUnlock()

	if stale {
		tm.kick(ctx, psn)
	}
}

func (tm *TelemetryManager) kick(ctx context.Context, psn uint64) {
	glog.Infof("telemetry: kicking psn=%d", psn)
	tm.mqtt.SendCommandNoResponse(ctx, psn, ServiceStartTelemetryStream, BuildStartTelemetryStream())
}

// run is the per-PSN goroutine: subscribe to stream data topics, send start command, cache updates.
func (tm *TelemetryManager) run(ctx context.Context, psn uint64, ts *telemetryStream) {
	portTopic := fmt.Sprintf(endUserResponseTopicTpl, psn, ServiceStreamPortStatus)
	deviceTopic := fmt.Sprintf(endUserResponseTopicTpl, psn, ServiceStreamDeviceStatus)

	// Subscribe first — must be in place before the device starts sending.
	tm.mqtt.router.RegisterHandler(portTopic, func(p *paho.Publish) {
		resp := &pb.CommandResponse{}
		if err := proto.Unmarshal(p.Payload, resp); err != nil {
			glog.Errorf("telemetry: unmarshal: psn=%d err=%v", psn, err)
			return
		}
		payload, ok := resp.Payload.(*pb.CommandResponse_StreamPortStatus)
		if !ok {
			return // ACK or other response — ignore
		}
		ts.latestMu.Lock()
		ts.latestPort = payload.StreamPortStatus
		ts.updatedPort = time.Now()
		ts.latestMu.Unlock()
	})
	tm.mqtt.router.RegisterHandler(deviceTopic, func(p *paho.Publish) {
		resp := &pb.CommandResponse{}
		if err := proto.Unmarshal(p.Payload, resp); err != nil {
			glog.Errorf("telemetry: unmarshal device: psn=%d err=%v", psn, err)
			return
		}
		payload, ok := resp.Payload.(*pb.CommandResponse_StreamDeviceStatus)
		if !ok {
			return // ACK or other response — ignore
		}
		ts.latestMu.Lock()
		ts.latestDevice = payload.StreamDeviceStatus
		ts.updatedDevice = time.Now()
		ts.latestMu.Unlock()
	})

	if err := tm.mqtt.addSubscription(ctx, portTopic); err != nil {
		tm.mqtt.router.UnregisterHandler(portTopic)
		tm.mqtt.router.UnregisterHandler(deviceTopic)
		glog.Errorf("telemetry: subscribe failed: psn=%d err=%v", psn, err)
		return
	}
	if err := tm.mqtt.addSubscription(ctx, deviceTopic); err != nil {
		tm.mqtt.router.UnregisterHandler(portTopic)
		tm.mqtt.router.UnregisterHandler(deviceTopic)
		tm.mqtt.removeSubscription(context.Background(), portTopic)
		glog.Errorf("telemetry: subscribe device failed: psn=%d err=%v", psn, err)
		return
	}

	// Fire start command with retries.
	for attempt := 1; attempt <= 3; attempt++ {
		tm.kick(ctx, psn)
		glog.Infof("telemetry: start sent psn=%d attempt=%d", psn, attempt)

		select {
		case <-ctx.Done():
			goto done
		case <-time.After(8 * time.Second):
		}

		ts.latestMu.RLock()
		ok := ts.latestPort != nil || ts.latestDevice != nil
		ts.latestMu.RUnlock()
		if ok {
			glog.Infof("telemetry: streaming psn=%d", psn)
			break
		}
		glog.Warningf("telemetry: no data after 8s psn=%d attempt=%d/3", psn, attempt)
	}

	<-ctx.Done()

done:
	tm.mqtt.router.UnregisterHandler(portTopic)
	tm.mqtt.router.UnregisterHandler(deviceTopic)
	tm.mqtt.removeSubscription(context.Background(), portTopic)
	tm.mqtt.removeSubscription(context.Background(), deviceTopic)
	tm.mqtt.SendCommandNoResponse(context.Background(), psn, ServiceStopTelemetryStream, BuildStopTelemetryStream())
	glog.Infof("telemetry: exiting psn=%d", psn)
}
