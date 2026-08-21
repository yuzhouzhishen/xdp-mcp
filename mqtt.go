package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/golang/glog"
	"google.golang.org/protobuf/proto"

	pb "xdp-mcp/proto/xdp/v1"
)

// Service ID constants (decimal)
const (
	ChargingStrategyUSBACharging pb.ChargingStrategy = 6
)

const (
	ServiceSetChargingStrategy     uint8 = 0x43 // 67
	ServiceGetChargingStatus       uint8 = 0x44 // 68
	ServiceGetPowerHistoricalStats uint8 = 0x45 // 69
	ServiceGetPortPDStatus         uint8 = 0x49 // 73
	ServiceTurnOnPort              uint8 = 0x4c // 76
	ServiceTurnOffPort             uint8 = 0x4d // 77
	ServiceSetTemperatureMode      uint8 = 0x5b // 91
	ServiceSetTemporaryAllocator   uint8 = 0x5c // 92
	ServiceSetDisplayIntensity     uint8 = 0x70 // 112
	ServiceSetDisplayMode          uint8 = 0x71 // 113
	ServiceGetDisplayIntensity     uint8 = 0x72 // 114
	ServiceGetDisplayMode          uint8 = 0x73 // 115
	ServiceSetDisplayConfigLegacy  uint8 = 0x76 // 118
	ServiceSetDisplayConfig        uint8 = 0x7f // 127
	ServiceStreamPortStatus        uint8 = 0x80 // 128 — stream data arrives here
	ServiceStreamDeviceStatus      uint8 = 0x81 // 129
	ServiceStreamPortPDStatus      uint8 = 0x82 // 130
	ServiceStartTelemetryStream    uint8 = 0x90 // 144 — start command + ACK
	ServiceStopTelemetryStream     uint8 = 0x91 // 145
	ServiceGetDeviceInfo           uint8 = 0x92 // 146
)

const (
	endUserRequestTopicTpl  = "device/%d/enduser/request/%d"
	endUserResponseTopicTpl = "device/%d/enduser/response/%d"
	commandTimeout          = 8 * time.Second
)

type pendingRequest struct {
	id uint32
	ch chan *pb.CommandResponse
}

type MQTTClient struct {
	conn   *autopaho.ConnectionManager
	router *paho.StandardRouter
	idSeq  atomic.Uint32

	mu       sync.Mutex
	pending  map[uint32]*pendingRequest
	subCount map[string]int
}

func NewMQTTClient(ctx context.Context, cfg *Config) (*MQTTClient, error) {
	brokerURL, err := url.Parse(cfg.MQTTBrokerURL)
	if err != nil {
		return nil, fmt.Errorf("parse mqtt broker url: %w", err)
	}

	c := &MQTTClient{
		pending:  make(map[uint32]*pendingRequest),
		subCount: make(map[string]int),
	}
	c.router = paho.NewStandardRouter()

	tlsCfg, err := cfg.MQTTTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("mqtt tls config: %w", err)
	}

	clientConfig := paho.ClientConfig{
		ClientID: cfg.MQTTClientID,
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){
			func(p paho.PublishReceived) (bool, error) {
				topic := p.Packet.Topic
				if strings.Contains(topic, "/enduser/response/") {
					resp := &pb.CommandResponse{}
					if err := proto.Unmarshal(p.Packet.Payload, resp); err == nil {
						c.mu.Lock()
						pending, ok := c.pending[resp.Id]
						c.mu.Unlock()
						if ok {
							select {
							case pending.ch <- resp:
							default:
							}
						}
					}
				}
				c.router.Route(p.Packet.Packet())
				return true, nil
			},
		},
		OnServerDisconnect: func(d *paho.Disconnect) {
			glog.Errorf("mqtt server disconnect: reason=%d", d.ReasonCode)
		},
		OnClientError: func(err error) {
			glog.Errorf("mqtt client error: %v", err)
		},
	}

	autoCfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{brokerURL},
		KeepAlive:                     20,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,
		TlsCfg:                        tlsCfg,
		ClientConfig:                  clientConfig,
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			glog.Infof("mqtt connection up: broker=%s", brokerURL.Host)
		},
		OnConnectError: func(err error) {
			glog.Errorf("mqtt connect error: %v", err)
		},
	}

	glog.Infof("connecting to mqtt broker %s as %s", brokerURL.Host, cfg.MQTTClientID)
	c.conn, err = autopaho.NewConnection(ctx, autoCfg)
	if err != nil {
		return nil, fmt.Errorf("mqtt connection: %w", err)
	}

	if err := c.conn.AwaitConnection(ctx); err != nil {
		return nil, fmt.Errorf("mqtt await connection: %w", err)
	}
	glog.Info("mqtt connection established")

	return c, nil
}

func (c *MQTTClient) addSubscription(ctx context.Context, topic string) error {
	c.mu.Lock()
	count := c.subCount[topic]
	c.subCount[topic]++
	c.mu.Unlock()

	if count == 0 {
		if _, err := c.conn.Subscribe(ctx, &paho.Subscribe{
			Subscriptions: []paho.SubscribeOptions{{Topic: topic, QoS: 1}},
		}); err != nil {
			c.mu.Lock()
			c.subCount[topic]--
			c.mu.Unlock()
			return err
		}
	}
	return nil
}

func (c *MQTTClient) removeSubscription(ctx context.Context, topic string) {
	c.mu.Lock()
	if c.subCount[topic] > 0 {
		c.subCount[topic]--
		count := c.subCount[topic]
		c.mu.Unlock()
		if count == 0 {
			c.conn.Unsubscribe(ctx, &paho.Unsubscribe{Topics: []string{topic}})
		}
	} else {
		c.mu.Unlock()
	}
}

func (c *MQTTClient) nextID() uint32 {
	return c.idSeq.Add(1)
}

// SendCommand publishes a CommandRequest and waits for matching CommandResponse.
func (c *MQTTClient) SendCommand(ctx context.Context, psn uint64, serviceID uint8, req *pb.CommandRequest) (*pb.CommandResponse, error) {
	id := c.nextID()
	req.Id = id

	responseTopic := fmt.Sprintf(endUserResponseTopicTpl, psn, serviceID)
	requestTopic := fmt.Sprintf(endUserRequestTopicTpl, psn, serviceID)

	glog.V(1).Infof("send command: psn=%d service=0x%02x id=%d topic=%s", psn, serviceID, id, requestTopic)

	// Register pending request
	pr := &pendingRequest{
		id: id,
		ch: make(chan *pb.CommandResponse, 1),
	}
	c.mu.Lock()
	c.pending[id] = pr
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	// Subscribe to response topic
	if err := c.addSubscription(ctx, responseTopic); err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", responseTopic, err)
	}

	defer c.removeSubscription(context.Background(), responseTopic)

	// Marshal and publish
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := c.conn.Publish(ctx, &paho.Publish{
			Topic:   requestTopic,
			QoS:     1,
			Payload: payload,
		}); err != nil {
			return nil, fmt.Errorf("publish %s: %w", requestTopic, err)
		}

		// Wait for response
		timeoutCtx, cancel := context.WithTimeout(ctx, commandTimeout)
		select {
		case resp := <-pr.ch:
			cancel()
			glog.V(1).Infof("command complete: psn=%d service=0x%02x id=%d status=%s", psn, serviceID, id, resp.Status)
			return resp, nil
		case <-timeoutCtx.Done():
			cancel()
			if attempt < maxAttempts {
				glog.Warningf("command timeout (attempt %d/%d): psn=%d service=0x%02x id=%d, retrying", attempt, maxAttempts, psn, serviceID, id)
				continue
			}
			glog.Warningf("command timeout: psn=%d service=0x%02x id=%d", psn, serviceID, id)
			return nil, fmt.Errorf("command timeout (service 0x%02x, id %d)", serviceID, id)
		}
	}

	// unreachable, but satisfy compiler
	return nil, fmt.Errorf("command failed")
}

// SendCommandNoResponse publishes a CommandRequest without waiting for a response.
func (c *MQTTClient) SendCommandNoResponse(ctx context.Context, psn uint64, serviceID uint8, req *pb.CommandRequest) error {
	id := c.nextID()
	req.Id = id

	requestTopic := fmt.Sprintf(endUserRequestTopicTpl, psn, serviceID)

	glog.V(1).Infof("send command (no response): psn=%d service=0x%02x id=%d topic=%s", psn, serviceID, id, requestTopic)

	payload, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if _, err := c.conn.Publish(ctx, &paho.Publish{
		Topic:   requestTopic,
		QoS:     1,
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("publish %s: %w", requestTopic, err)
	}

	return nil
}

// Command builders

func BuildGetDeviceInfo() *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_GetDeviceInfo{
			GetDeviceInfo: &pb.GetDeviceInfoReq{},
		},
	}
}

func BuildStartTelemetryStream() *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_StartTelemetryStream{
			StartTelemetryStream: &pb.StartTelemetryStreamReq{},
		},
	}
}

func BuildStopTelemetryStream() *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_StopTelemetryStream{
			StopTelemetryStream: &pb.StopTelemetryStreamReq{},
		},
	}
}

func BuildSetChargingStrategy(strategy pb.ChargingStrategy) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_SetChargingStrategy{
			SetChargingStrategy: &pb.SetChargingStrategyReq{
				ChargingStrategy: strategy,
			},
		},
	}
}

func BuildGetChargingStatus() *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_GetChargingStatus{
			GetChargingStatus: &pb.GetChargingStatusReq{},
		},
	}
}

func BuildTurnOnPort(ports []uint32) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_TurnOnPort{
			TurnOnPort: &pb.TurnOnPortReq{
				Ports: ports,
			},
		},
	}
}

func BuildTurnOffPort(ports []uint32) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_TurnOffPort{
			TurnOffPort: &pb.TurnOffPortReq{
				Ports: ports,
			},
		},
	}
}

func BuildSetTemperatureMode(mode pb.TemperatureMode) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_SetTemperatureMode{
			SetTemperatureMode: &pb.SetTemperatureModeReq{
				Mode: mode,
			},
		},
	}
}

func BuildSetTemporaryAllocator(powerAllocation []uint32) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_SetTemporaryAllocator{
			SetTemporaryAllocator: &pb.SetTemporaryAllocatorReq{
				PowerAllocation: powerAllocation,
			},
		},
	}
}

func BuildGetPortPDStatus(port uint32) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_GetPortPdStatus{
			GetPortPdStatus: &pb.GetPortPDStatusReq{
				Port: port,
			},
		},
	}
}

func BuildGetPowerHistoricalStats(port, offset uint32) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_GetPowerHistoricalStats{
			GetPowerHistoricalStats: &pb.GetPowerHistoricalStatsReq{
				Port:   port,
				Offset: offset,
			},
		},
	}
}

func BuildSetDisplayIntensity(intensity uint32) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_SetDisplayIntensity{
			SetDisplayIntensity: &pb.SetDisplayIntensityReq{
				Intensity: intensity,
			},
		},
	}
}

func BuildGetDisplayIntensity() *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_GetDisplayIntensity{
			GetDisplayIntensity: &pb.GetDisplayIntensityReq{},
		},
	}
}

func BuildSetDisplayConfig(config *pb.DisplayConfig) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_DisplayConfig{
			DisplayConfig: &pb.SetDisplayConfigReq{
				Config: config,
			},
		},
	}
}

func BuildSetDisplayConfigLegacy(config *pb.DisplayConfig) *pb.CommandRequest {
	return &pb.CommandRequest{
		Payload: &pb.CommandRequest_SetDisplayConfig{
			SetDisplayConfig: &pb.SetDisplayConfigDepreReq{
				Config: &pb.DisplayConfiguration{
					Intensity:                 config.GetIntensity(),
					Mode:                      config.GetMode(),
					Flip:                      config.GetFlip(),
					Rotation:                  config.GetRotation(),
					IdleAnimation:             config.GetIdleAnimation(),
					ChargingAnimationDuration: config.GetChargingAnimationDuration(),
					HourlyChime:               proto.Bool(config.GetHourlyChime()),
				},
			},
		},
	}
}
