package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"google.golang.org/protobuf/proto"

	pb "xdp-mcp/proto/xdp/v1"
)

//go:embed data/skills/*.md
var skillsFS embed.FS

//go:embed data/machine_facts/*.json
var machineFactsFS embed.FS

//go:embed data/device_database.csv
var deviceDatabaseCSV []byte

//go:embed data/cable_database.csv
var cableDatabaseCSV []byte

//go:embed data/prompts/*.txt
var promptsFS embed.FS

type deviceEntry struct {
	BrandEN string
	BrandZH string
	NameEN  string
	NameZH  string
}

// deviceDB maps "vid:pid" → deviceEntry, loaded once at init.
var deviceDB map[string]deviceEntry

// cableDB maps "vid:pid" → deviceEntry, loaded once at init.
var cableDB map[string]deviceEntry

const (
	minUserPort    = 1
	maxDevicePorts = 32
)

type displayBrightnessLevel struct {
	Name      string
	Intensity uint32
	Aliases   []uint32
}

type cp02sDisplayModeOption struct {
	Name     string
	Duration uint32
}

type cp02sIdleDisplayOption struct {
	Name      string
	Animation pb.IdleAnimationType
}

var cp02DisplayBrightnessLevels = []displayBrightnessLevel{
	{Name: "关", Intensity: 0, Aliases: []uint32{0}},
	{Name: "低", Intensity: 5, Aliases: []uint32{5, 20}},
	{Name: "中", Intensity: 15, Aliases: []uint32{15, 50}},
	{Name: "高", Intensity: 100, Aliases: []uint32{100, 128}},
}

var cp02sDisplayBrightnessLevels = []displayBrightnessLevel{
	{Name: "关", Intensity: 0, Aliases: []uint32{0}},
	{Name: "低", Intensity: 5, Aliases: []uint32{5}},
	{Name: "中", Intensity: 15, Aliases: []uint32{15}},
	{Name: "高", Intensity: 30, Aliases: []uint32{30, 100, 128}},
}

var cp02sDisplayModeOptions = []cp02sDisplayModeOption{
	{Name: "待机动画优先", Duration: 30},
	{Name: "功率显示优先", Duration: 0},
}

var cp02sIdleDisplayOptions = []cp02sIdleDisplayOption{
	{Name: "流星", Animation: pb.IdleAnimationType_IDLE_METEOR},
	{Name: "落花", Animation: pb.IdleAnimationType_IDLE_RIPPLE},
	{Name: "康威的生命游戏", Animation: pb.IdleAnimationType_IDLE_GAME_OF_LIFE},
	{Name: "时间", Animation: pb.IdleAnimationType_IDLE_RAIN},
}

func init() {
	deviceDB = loadDeviceDatabase(deviceDatabaseCSV)
	cableDB = loadDeviceDatabase(cableDatabaseCSV)
	glog.Infof("loaded %d cable database entries", len(cableDB))
}

func loadDeviceDatabase(data []byte) map[string]deviceEntry {
	db := make(map[string]deviceEntry)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			continue // skip header
		}
		r := csv.NewReader(strings.NewReader(line))
		fields, err := r.Read()
		if err != nil || len(fields) < 6 {
			continue
		}
		pid, err1 := strconv.ParseUint(strings.TrimPrefix(fields[0], "0x"), 16, 32)
		vid, err2 := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		key := fmt.Sprintf("%d:%d", vid, pid)
		db[key] = deviceEntry{
			BrandEN: strings.TrimSpace(fields[2]),
			BrandZH: strings.TrimSpace(fields[3]),
			NameEN:  strings.TrimSpace(fields[4]),
			NameZH:  strings.TrimSpace(fields[5]),
		}
	}
	glog.Infof("loaded %d device database entries", len(db))
	return db
}

func lookupDevice(vid, pid uint32) (deviceEntry, bool) {
	key := fmt.Sprintf("%d:%d", vid, pid)
	e, ok := deviceDB[key]
	return e, ok
}

func lookupCable(vid, pid uint32) (deviceEntry, bool) {
	key := fmt.Sprintf("%d:%d", vid, pid)
	e, ok := cableDB[key]
	return e, ok
}

// USB PD spec decoders (USB PD 3.2 v1.1 / Type-C 2.4)

func decodeCableMaxVbusCurrent(v uint32) string {
	switch v {
	case 0:
		return "3A"
	case 1:
		return "5A"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

func decodeCableMaxVbusVoltage(v uint32) string {
	switch v {
	case 0:
		return "20V"
	case 1:
		return "50V"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

func decodeCableUSBHighestSpeed(v uint32) string {
	switch v {
	case 0:
		return "USB 2.0"
	case 1:
		return "USB 3.2 Gen1"
	case 2:
		return "USB 3.2 Gen2"
	case 3:
		return "USB4 Gen2"
	case 4:
		return "USB4 Gen3"
	case 5:
		return "USB4 Gen4"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

func decodePDRevision(v uint32) string {
	switch v {
	case 0:
		return "PD 1.0"
	case 1:
		return "PD 2.0"
	case 2:
		return "PD 3.0"
	case 3:
		return "PD 3.1+"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

func decodeCableActiveElement(v uint32) string {
	switch v {
	case 0:
		return "passive"
	case 1:
		return "re-driver"
	case 2:
		return "re-timer"
	case 3:
		return "LRD"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

func decodeCableLatency(v uint32) string {
	switch v {
	case 0:
		return "<10ns"
	case 1:
		return "10-20ns"
	case 2:
		return "20-30ns"
	case 3:
		return ">30ns"
	default:
		return fmt.Sprintf("unknown(%d)", v)
	}
}

// tempRange converts a raw temperature value to a redacted range label.
func tempRange(t uint32) string {
	switch {
	case t < 40:
		return "cool"
	case t <= 70:
		return "moderate"
	default:
		return "warm"
	}
}

func decodeFCProtocol(p pb.FastChargingProtocol) string {
	switch p {
	case pb.FastChargingProtocol_NONE:
		return "USB Legacy Charging"
	case pb.FastChargingProtocol_QC2:
		return "Qualcomm QuickCharge 2.0"
	case pb.FastChargingProtocol_QC3:
		return "Qualcomm QuickCharge 3.0"
	case pb.FastChargingProtocol_QC3P:
		return "Qualcomm QuickCharge 3+"
	case pb.FastChargingProtocol_SFCP:
		return "SFCP"
	case pb.FastChargingProtocol_AFC:
		return "AFC"
	case pb.FastChargingProtocol_FCP:
		return "Huawei FCP"
	case pb.FastChargingProtocol_SCP:
		return "Huawei SCP"
	case pb.FastChargingProtocol_VOOC1P0:
		return "VOOC 1.0"
	case pb.FastChargingProtocol_VOOC4P0:
		return "VOOC 4.0"
	case pb.FastChargingProtocol_SVOOC2P0:
		return "SuperVOOC 2.0"
	case pb.FastChargingProtocol_TFCP:
		return "TFCP"
	case pb.FastChargingProtocol_UFCS:
		return "UFCS"
	case pb.FastChargingProtocol_PE1:
		return "MediaTek PE 1.0"
	case pb.FastChargingProtocol_PE2:
		return "MediaTek PE 2.0"
	case pb.FastChargingProtocol_PD_FIX5V:
		return "PD Fixed 5V"
	case pb.FastChargingProtocol_PD_FIXHV:
		return "PD Fixed High Voltage"
	case pb.FastChargingProtocol_PD_SPR_AVS:
		return "PD SPR AVS"
	case pb.FastChargingProtocol_PD_PPS:
		return "PD Programmable Power Supply"
	case pb.FastChargingProtocol_PD_EPR_HV:
		return "PD EPR High Voltage"
	case pb.FastChargingProtocol_PD_AVS:
		return "PD EPR AVS"
	case pb.FastChargingProtocol(21):
		return "小米澎湃秒充"
	case pb.FastChargingProtocol_NOT_CHARGING:
		return "Not Charging"
	default:
		return fmt.Sprintf("unknown(%d)", int32(p))
	}
}

// psnFromContext extracts PSN from the tool call context.
// The MCP library propagates the context set via SSEContextFunc.
func psnFromContext(ctx context.Context) (uint64, error) {
	psn, ok := ctx.Value(ctxKeyPSN).(uint64)
	if !ok {
		return 0, fmt.Errorf("no PSN in context")
	}
	return psn, nil
}

// promptDescriptions provides human-readable descriptions for each prompt.
var promptDescriptions = map[string]string{
	"charging_brief":      "Quick glance at current charging state — device, ports, power, protocols",
	"charging_status":     "Detailed charging status with PD details, cable info, and optional trend analysis",
	"charging_control":    "Control charging: change strategy, temperature mode, port on/off, power allocation",
	"charging_profile":    "Record a charging session profile over time with periodic data capture and graphing",
	"charging_comparison": "Compare charging performance across ports, before/after changes, or over time",
	"device_diagnostic":   "Deep device and cable diagnostics with compatibility analysis and recommendations",
}

func registerPrompts(s *server.MCPServer) {
	entries, err := fs.ReadDir(promptsFS, "data/prompts")
	if err != nil {
		glog.Errorf("read prompts dir: %v", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		content, err := fs.ReadFile(promptsFS, "data/prompts/"+e.Name())
		if err != nil {
			glog.Errorf("read prompt %s: %v", e.Name(), err)
			continue
		}
		desc := promptDescriptions[name]
		if desc == "" {
			desc = name
		}
		promptContent := string(content)

		// Register as MCP prompt
		s.AddPrompt(
			mcp.NewPrompt(name, mcp.WithPromptDescription(desc)),
			func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{
					Messages: []mcp.PromptMessage{
						{
							Role:    mcp.RoleUser,
							Content: mcp.TextContent{Type: "text", Text: promptContent},
						},
					},
				}, nil
			},
		)

		// Register as MCP resource (so clients like Codex can discover via resources/list)
		resourceURI := "prompt:///" + name
		s.AddResource(
			mcp.NewResource(resourceURI, name,
				mcp.WithResourceDescription(desc),
				mcp.WithMIMEType("text/plain"),
			),
			func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return []mcp.ResourceContents{
					mcp.TextResourceContents{
						URI:      resourceURI,
						MIMEType: "text/plain",
						Text:     promptContent,
					},
				}, nil
			},
		)

		glog.Infof("registered prompt + resource: %s", name)
	}
}

func registerAllToolsWithContext(s *server.MCPServer, mqtt *MQTTClient, pool *pgxpool.Pool, _ *sync.Map, telemetry *TelemetryManager) {

	// Register prompts from embedded FS
	registerPrompts(s)

	// Tool 2: get_device_info
	s.AddTool(
		mcp.NewTool("get_device_info",
			mcp.WithDescription("Get device info including PSN, model, firmware versions, WiFi details, and MAC addresses. No input required."),
		),
		makeGetDeviceInfoHandler(mqtt),
	)

	// Tool 2b: get_machine_facts
	s.AddTool(
		mcp.NewTool("get_machine_facts",
			mcp.WithDescription("Get static machine facts (port count, port types, max power budget, etc.) for this device. No input required."),
		),
		makeGetMachineFactsHandler(pool),
	)

	// Tool 2c: get_port_details
	s.AddTool(
		mcp.NewTool("get_port_details",
			mcp.WithDescription("Get live port details for all ports: voltage, current, power budget, fast-charge protocol, connection status, and device identification. No input required."),
		),
		makeGetPortDetailsHandler(mqtt, telemetry),
	)

	// Tool 3: set_charging_strategy
	s.AddTool(
		mcp.NewTool("set_charging_strategy",
			mcp.WithDescription("Set the charging strategy for the device."),
			mcp.WithNumber("strategy",
				mcp.Required(),
				mcp.Description("Charging strategy: 0=FAST/自由流/FluxAI自由流/超速充, 1=SLOW, 6=USBA_CHARGING/小家电模式/模拟A口/魔拟充/模拟充, 7=HIGH_PERFORMANCE, 8=ULTRA_FAST_SINGLE_PORT"),
			),
		),
		makeSetChargingStrategyHandler(mqtt),
	)

	// Tool 3b: set_usba_charging_mode
	s.AddTool(
		mcp.NewTool("set_usba_charging_mode",
			mcp.WithDescription("Switch to USBA charging mode. Use this when the user says 切换魔拟充、小家电模式、模拟 A 口、模拟A口、模拟充."),
		),
		makeSetUSBAChargingModeHandler(mqtt),
	)

	// Tool 4: get_charging_status
	s.AddTool(
		mcp.NewTool("get_charging_status",
			mcp.WithDescription("Get the charging status bitmask. Bit N set means port N+1 is currently charging. No input required."),
		),
		makeGetChargingStatusHandler(mqtt),
	)

	// Tool 5: get_temperature_mode
	s.AddTool(
		mcp.NewTool("get_temperature_mode",
			mcp.WithDescription("Get the global temperature mode for the device. No input required."),
		),
		makeGetTemperatureModeHandler(telemetry),
	)

	// Tool 6: set_temperature_mode
	s.AddTool(
		mcp.NewTool("set_temperature_mode",
			mcp.WithDescription("Set the temperature mode for the device."),
			mcp.WithNumber("mode",
				mcp.Required(),
				mcp.Description("Temperature mode: 0=POWER_PRIORITY, 1=TEMPERATURE_PRIORITY"),
			),
		),
		makeSetTemperatureModeHandler(mqtt),
	)

	// Tool 7: set_port_power_allocation
	s.AddTool(
		mcp.NewTool("set_port_power_allocation",
			mcp.WithDescription("Set temporary per-port power allocation values (in watts)."),
			mcp.WithArray("power_allocation",
				mcp.Required(),
				mcp.Description("Array of per-port power values in watts (uint32)"),
			),
		),
		makeSetTemporaryAllocatorHandler(mqtt),
	)

	// Tool 8: turn_on_port
	s.AddTool(
		mcp.NewTool("turn_on_port",
			mcp.WithDescription("Turn on one or more charging ports."),
			mcp.WithArray("ports",
				mcp.Required(),
				mcp.Description("Array of port numbers to turn on (1-indexed)"),
			),
		),
		makeTurnOnPortHandler(mqtt),
	)

	// Tool 9: turn_off_port
	s.AddTool(
		mcp.NewTool("turn_off_port",
			mcp.WithDescription("Turn off one or more charging ports."),
			mcp.WithArray("ports",
				mcp.Required(),
				mcp.Description("Array of port numbers to turn off (1-indexed)"),
			),
		),
		makeTurnOffPortHandler(mqtt),
	)

	// Tool 10: get_port_stats
	s.AddTool(
		mcp.NewTool("get_port_stats",
			mcp.WithDescription("Get historical power stats for a port from device memory. Returns current/voltage timeseries. Auto-paginates to fetch all available data."),
			mcp.WithNumber("port",
				mcp.Required(),
				mcp.Description("Port number (1-indexed)"),
			),
		),
		makeGetPortStatsHandler(mqtt),
	)

	// Tool 11: get_port_pd_status
	s.AddTool(
		mcp.NewTool("get_port_pd_status",
			mcp.WithDescription("Get USB PD (Power Delivery) status for all charging ports. Internally discovers active ports from charging status, then fetches PD info in parallel. No input required."),
		),
		makeGetPortPDStatusHandler(mqtt),
	)

	// Tool 14: get_display_config
	s.AddTool(
		mcp.NewTool("get_display_config",
			mcp.WithDescription("Get current screen/status display brightness. CP02S also returns display mode, idle display, and hourly chime. No input required."),
		),
		makeGetDisplayConfigHandler(mqtt, pool, telemetry),
	)

	// Tool 15: set_display_intensity
	s.AddTool(
		mcp.NewTool("set_display_intensity",
			mcp.WithDescription("Set screen/status display brightness level (状态屏亮度). Use one of: 关, 低, 中, 高."),
			mcp.WithString("level",
				mcp.Required(),
				mcp.Enum("关", "低", "中", "高"),
				mcp.Description("Screen/status display brightness level: 关, 低, 中, 高"),
			),
		),
		makeSetDisplayIntensityHandler(mqtt, pool),
	)

	// Tool 16: set_status_display_mode
	s.AddTool(
		mcp.NewTool("set_status_display_mode",
			mcp.WithDescription("Set CP02S status display mode (屏显模式). Use one of: 待机动画优先, 功率显示优先."),
			mcp.WithString("mode",
				mcp.Required(),
				mcp.Enum("待机动画优先", "功率显示优先"),
				mcp.Description("Status display mode: 待机动画优先 or 功率显示优先"),
			),
		),
		makeSetStatusDisplayModeHandler(mqtt, pool, telemetry),
	)

	// Tool 17: set_idle_display
	s.AddTool(
		mcp.NewTool("set_idle_display",
			mcp.WithDescription("Set CP02S idle display (待机显示). Use one of: 流星, 落花, 康威的生命游戏, 时间."),
			mcp.WithString("idle_display",
				mcp.Required(),
				mcp.Enum("流星", "落花", "康威的生命游戏", "时间"),
				mcp.Description("Idle display: 流星, 落花, 康威的生命游戏, 时间"),
			),
		),
		makeSetIdleDisplayHandler(mqtt, pool, telemetry),
	)

	// Tool 18: set_hourly_chime
	s.AddTool(
		mcp.NewTool("set_hourly_chime",
			mcp.WithDescription("Enable or disable CP02S hourly chime (整点报时). Keeps the current idle display."),
			mcp.WithBoolean("enabled",
				mcp.Required(),
				mcp.Description("Whether hourly chime should be enabled"),
			),
		),
		makeSetHourlyChimeHandler(mqtt, pool, telemetry),
	)
}

// Tool 2: get_device_info
func makeGetDeviceInfoHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		glog.Infof("get_device_info: psn=%d", psn)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceGetDeviceInfo, BuildGetDeviceInfo())
		if err != nil {
			glog.Warningf("get_device_info: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("get device info: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("get_device_info: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		infoPayload, ok := resp.Payload.(*pb.CommandResponse_GetDeviceInfo)
		if !ok {
			return mcp.NewToolResultError("unexpected response type for get_device_info"), nil
		}
		info := infoPayload.GetDeviceInfo.Info

		result := map[string]any{
			"psn":           info.GetPsn(),
			"model":         info.GetModel(),
			"app_version":   fmtVersion(info.GetApVersion()),
			"fpga_version":  fmtVersion(info.GetFpgaVersion()),
			"bssid":         info.GetBssid(),
			"ssid":          info.GetSsid(),
			"rssi":          info.GetRssi(),
			"wifi_protocol": info.GetWifiProtocol(),
			"channel":       info.GetChannel(),
		}

		data, _ := json.Marshal(result)
		glog.Infof("get_device_info: ok psn=%d", psn)
		return mcp.NewToolResultText(string(data)), nil
	}
}

func fmtVersion(v *pb.Version) string {
	if v == nil {
		return "0.0.0"
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Tool 2b: get_machine_facts
func makeGetMachineFactsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		var productFamily string
		err = pool.QueryRow(ctx,
			`SELECT product_family FROM device_information WHERE psn = $1`, psn,
		).Scan(&productFamily)
		if err != nil {
			glog.Warningf("get_machine_facts: db lookup failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("lookup product family: %v", err)), nil
		}

		productFamily = strings.ToLower(strings.TrimSpace(productFamily))
		path := fmt.Sprintf("data/machine_facts/%s.json", productFamily)

		data, err := machineFactsFS.ReadFile(path)
		if err != nil {
			glog.Warningf("get_machine_facts: psn=%d product_family=%s err=%v", psn, productFamily, err)
			return mcp.NewToolResultError(fmt.Sprintf("unknown product family: %s", productFamily)), nil
		}

		glog.Infof("get_machine_facts: psn=%d product_family=%s", psn, productFamily)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 2c: get_port_details
func makeGetPortDetailsHandler(mqtt *MQTTClient, telemetry *TelemetryManager) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		glog.Infof("get_port_details: psn=%d checking telemetry cache", psn)

		// Try cached telemetry first (updated within 30s)
		var streamData *pb.StreamPortStatus
		if cached, updated := telemetry.Latest(psn); cached != nil && time.Since(updated) < 30*time.Second {
			glog.Infof("get_port_details: psn=%d using cached data (age=%v)", psn, time.Since(updated))
			streamData = cached
		} else {
			// Kick telemetry to re-send start command, then poll cache
			glog.Infof("get_port_details: psn=%d cache miss, kicking telemetry", psn)
			telemetry.Kick(ctx, psn)

			// Poll cache for up to 10s
			deadline := time.After(10 * time.Second)
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
		poll:
			for {
				select {
				case <-ticker.C:
					if cached, updated := telemetry.Latest(psn); cached != nil && time.Since(updated) < 30*time.Second {
						glog.Infof("get_port_details: psn=%d got cached data after wait (age=%v)", psn, time.Since(updated))
						streamData = cached
						break poll
					}
				case <-deadline:
					glog.Warningf("get_port_details: psn=%d telemetry cache timeout", psn)
					return mcp.NewToolResultError("telemetry data unavailable — device may be offline"), nil
				case <-ctx.Done():
					return mcp.NewToolResultError("request cancelled"), nil
				}
			}
		}

		var ports []any
		for i, p := range streamData.Ports {
			d := p.GetDetails()
			if d == nil {
				continue
			}
			entry := map[string]any{
				"port":               i + 1, // 1-indexed
				"connected":          d.GetConnected(),
				"iout_ma":            d.GetIoutValue(),
				"vout_mv":            d.GetVoutValue(),
				"vin_mv":             d.GetVinValue(),
				"die_temperature":    tempRange(d.GetDieTemperature()),
				"fc_protocol":        decodeFCProtocol(d.GetFcProtocol()),
				"session_charge_mwh": d.GetSessionCharge() / 3600000.0,
				"session_id":         d.GetSessionId(),
				"manufacturer_vid":   d.GetManufacturerVid(),
				"manufacturer_pid":   d.GetManufacturerPid(),
			}
			if dev, ok := lookupDevice(d.GetManufacturerVid(), d.GetManufacturerPid()); ok {
				delete(entry, "manufacturer_vid")
				delete(entry, "manufacturer_pid")
				entry["device_brand_en"] = dev.BrandEN
				entry["device_brand_zh"] = dev.BrandZH
				entry["device_name_en"] = dev.NameEN
				entry["device_name_zh"] = dev.NameZH
			}
			ports = append(ports, entry)
		}

		data, _ := json.Marshal(map[string]any{"ports": ports})
		glog.Infof("get_port_details: ok psn=%d ports=%d", psn, len(ports))
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 3: set_charging_strategy
func makeSetChargingStrategyHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		strategyVal, err := request.RequireInt("strategy")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		strategy := pb.ChargingStrategy(strategyVal)
		strategyName := chargingStrategyDisplayName(strategy)
		glog.Infof("set_charging_strategy: psn=%d strategy=%s", psn, strategyName)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceSetChargingStrategy, BuildSetChargingStrategy(strategy))
		if err != nil {
			glog.Warningf("set_charging_strategy: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("set charging strategy: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("set_charging_strategy: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		glog.Infof("set_charging_strategy: ok psn=%d strategy=%s", psn, strategyName)
		return mcp.NewToolResultText(fmt.Sprintf("charging strategy set to %s", strategyName)), nil
	}
}

func makeSetUSBAChargingModeHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		strategy := ChargingStrategyUSBACharging
		strategyName := chargingStrategyDisplayName(strategy)
		glog.Infof("set_usba_charging_mode: psn=%d strategy=%s", psn, strategyName)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceSetChargingStrategy, BuildSetChargingStrategy(strategy))
		if err != nil {
			glog.Warningf("set_usba_charging_mode: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("set USBA charging mode: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("set_usba_charging_mode: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		glog.Infof("set_usba_charging_mode: ok psn=%d strategy=%s", psn, strategyName)
		return mcp.NewToolResultText("USBA charging mode set"), nil
	}
}

func chargingStrategyDisplayName(strategy pb.ChargingStrategy) string {
	switch strategy {
	case ChargingStrategyUSBACharging:
		return "USBA_CHARGING"
	default:
		return strategy.String()
	}
}

// Tool 4: get_charging_status
func makeGetChargingStatusHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		glog.Infof("get_charging_status: psn=%d", psn)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceGetChargingStatus, BuildGetChargingStatus())
		if err != nil {
			glog.Warningf("get_charging_status: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("get charging status: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("get_charging_status: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		statusPayload, ok := resp.Payload.(*pb.CommandResponse_GetChargingStatus)
		if !ok {
			return mcp.NewToolResultError("unexpected response type"), nil
		}

		statusBitmask := statusPayload.GetChargingStatus.Status
		ports := make(map[string]bool)
		for i := 0; i < maxDevicePorts; i++ {
			if statusBitmask&(1<<uint(i)) != 0 {
				ports[fmt.Sprintf("port_%d", i+1)] = true
			}
		}

		result := map[string]any{
			"status_bitmask": statusBitmask,
			"charging_ports": ports,
		}
		data, _ := json.Marshal(result)
		glog.Infof("get_charging_status: psn=%d bitmask=0x%x", psn, statusBitmask)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 5: get_temperature_mode
func makeGetTemperatureModeHandler(telemetry *TelemetryManager) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		streamData, updated := telemetry.LatestDevice(psn)
		if streamData == nil || time.Since(updated) > 30*time.Second {
			glog.Infof("get_temperature_mode: psn=%d cache miss/stale, triggering telemetry", psn)
			telemetry.KickDevice(ctx, psn)

			deadline := time.After(10 * time.Second)
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
		poll:
			for {
				select {
				case <-ticker.C:
					if cached, updated := telemetry.LatestDevice(psn); cached != nil && time.Since(updated) < 30*time.Second {
						glog.Infof("get_temperature_mode: psn=%d got cached data after wait (age=%v)", psn, time.Since(updated))
						streamData = cached
						break poll
					}
				case <-deadline:
					glog.Warningf("get_temperature_mode: psn=%d telemetry cache timeout", psn)
					return mcp.NewToolResultError("temperature mode unavailable — device may be offline"), nil
				case <-ctx.Done():
					return mcp.NewToolResultError("request cancelled"), nil
				}
			}
		}

		mode := streamData.GetTemperatureMode()
		data, _ := json.Marshal(map[string]any{
			"mode":      int32(mode),
			"mode_name": mode.String(),
		})
		glog.Infof("get_temperature_mode: ok psn=%d mode=%s", psn, mode)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 6: set_temperature_mode
func makeSetTemperatureModeHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		modeVal, err := request.RequireInt("mode")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		mode := pb.TemperatureMode(modeVal)
		glog.Infof("set_temperature_mode: psn=%d mode=%s", psn, mode)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceSetTemperatureMode, BuildSetTemperatureMode(mode))
		if err != nil {
			glog.Warningf("set_temperature_mode: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("set temperature mode: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("set_temperature_mode: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		glog.Infof("set_temperature_mode: ok psn=%d mode=%s", psn, mode)
		return mcp.NewToolResultText(fmt.Sprintf("temperature mode set to %s", mode)), nil
	}
}

// Tool 6: set_port_power_allocation
func makeSetTemporaryAllocatorHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		args := request.GetArguments()
		rawAlloc, ok := args["power_allocation"]
		if !ok {
			return mcp.NewToolResultError("power_allocation is required"), nil
		}

		allocSlice, ok := rawAlloc.([]any)
		if !ok {
			return mcp.NewToolResultError("power_allocation must be an array"), nil
		}

		allocation := make([]uint32, len(allocSlice))
		for i, v := range allocSlice {
			switch n := v.(type) {
			case float64:
				if n < 0 {
					return mcp.NewToolResultError(fmt.Sprintf("invalid power value at index %d", i)), nil
				}
				allocation[i] = uint32(n)
			case json.Number:
				val, err := n.Int64()
				if err != nil || val < 0 {
					return mcp.NewToolResultError(fmt.Sprintf("invalid power value at index %d", i)), nil
				}
				allocation[i] = uint32(val)
			default:
				return mcp.NewToolResultError(fmt.Sprintf("invalid power value type at index %d", i)), nil
			}
		}

		glog.Infof("set_port_power_allocation: psn=%d allocation=%v", psn, allocation)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceSetTemporaryAllocator, BuildSetTemporaryAllocator(allocation))
		if err != nil {
			glog.Warningf("set_port_power_allocation: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("set temporary allocator: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("set_port_power_allocation: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		glog.Infof("set_port_power_allocation: ok psn=%d", psn)
		return mcp.NewToolResultText("temporary power allocation set"), nil
	}
}

// Tool 7: turn_on_port
func makeTurnOnPortHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		userPorts, err := parsePortsArg(request)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Convert 1-indexed user ports to 0-indexed device ports
		devicePorts := make([]uint32, len(userPorts))
		for i, p := range userPorts {
			if p < minUserPort {
				return mcp.NewToolResultError(fmt.Sprintf("port %d must be >= %d (1-indexed)", p, minUserPort)), nil
			}
			devicePorts[i] = p - 1
		}

		glog.Infof("turn_on_port: psn=%d ports=%v (device=%v)", psn, userPorts, devicePorts)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceTurnOnPort, BuildTurnOnPort(devicePorts))
		if err != nil {
			glog.Warningf("turn_on_port: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("turn on port: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("turn_on_port: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		glog.Infof("turn_on_port: ok psn=%d ports=%v", psn, userPorts)
		return mcp.NewToolResultText(fmt.Sprintf("ports %v turned on", userPorts)), nil
	}
}

// Tool 8: turn_off_port
func makeTurnOffPortHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		userPorts, err := parsePortsArg(request)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Convert 1-indexed user ports to 0-indexed device ports
		devicePorts := make([]uint32, len(userPorts))
		for i, p := range userPorts {
			if p < minUserPort {
				return mcp.NewToolResultError(fmt.Sprintf("port %d must be >= %d (1-indexed)", p, minUserPort)), nil
			}
			devicePorts[i] = p - 1
		}

		glog.Infof("turn_off_port: psn=%d ports=%v (device=%v)", psn, userPorts, devicePorts)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceTurnOffPort, BuildTurnOffPort(devicePorts))
		if err != nil {
			glog.Warningf("turn_off_port: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("turn off port: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			glog.Warningf("turn_off_port: device returned status=%s psn=%d", resp.Status, psn)
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		glog.Infof("turn_off_port: ok psn=%d ports=%v", psn, userPorts)
		return mcp.NewToolResultText(fmt.Sprintf("ports %v turned off", userPorts)), nil
	}
}

// Tool 9: get_port_stats
func makeGetPortStatsHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		portNum, err := request.RequireInt("port")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if portNum < minUserPort {
			return mcp.NewToolResultError(fmt.Sprintf("port must be >= %d (1-indexed)", minUserPort)), nil
		}
		port := uint32(portNum - 1) // device uses 0-indexed

		glog.Infof("get_port_stats: psn=%d port=%d (device_port=%d)", psn, portNum, port)

		// Auto-paginate: fetch all pages from device memory
		var allSamples []map[string]any
		offset := uint32(0)
		const maxPages = 10
		for page := 0; page < maxPages; page++ {
			resp, err := mqtt.SendCommand(ctx, psn, ServiceGetPowerHistoricalStats, BuildGetPowerHistoricalStats(port, offset))
			if err != nil {
				if page == 0 {
					glog.Warningf("get_port_stats: failed: psn=%d err=%v", psn, err)
					return mcp.NewToolResultError(fmt.Sprintf("get power historical stats: %v", err)), nil
				}
				break // partial data is ok
			}
			if resp.Status != pb.CommandStatus_SUCCESS {
				if page == 0 {
					return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
				}
				break
			}

			statsResp, ok := resp.Payload.(*pb.CommandResponse_GetPowerHistoricalStats)
			if !ok || len(statsResp.GetPowerHistoricalStats.GetData()) == 0 {
				break // no more data
			}

			for _, d := range statsResp.GetPowerHistoricalStats.GetData() {
				powerMw := d.GetCurrent() * d.GetVoltage() / 1000
				allSamples = append(allSamples, map[string]any{
					"current_ma": d.GetCurrent(),
					"voltage_mv": d.GetVoltage(),
					"power_mw":   powerMw,
				})
			}

			nextOffset := statsResp.GetPowerHistoricalStats.GetOffset()
			if nextOffset == 0 || nextOffset == offset {
				break // no more pages
			}
			offset = nextOffset
		}

		result := map[string]any{
			"port":    portNum,
			"samples": allSamples,
			"count":   len(allSamples),
		}

		data, _ := json.Marshal(result)
		glog.Infof("get_port_stats: ok psn=%d port=%d samples=%d", psn, portNum, len(allSamples))
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 10: get_port_pd_status
func makeGetPortPDStatusHandler(mqtt *MQTTClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		glog.Infof("get_port_pd_status: psn=%d fetching charging status", psn)

		// Step 1: get charging status to find active ports
		statusResp, err := mqtt.SendCommand(ctx, psn, ServiceGetChargingStatus, BuildGetChargingStatus())
		if err != nil {
			glog.Warningf("get_port_pd_status: charging status failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("get charging status: %v", err)), nil
		}
		if statusResp.Status != pb.CommandStatus_SUCCESS {
			return mcp.NewToolResultError(fmt.Sprintf("charging status returned: %s", statusResp.Status)), nil
		}

		statusPayload, ok := statusResp.Payload.(*pb.CommandResponse_GetChargingStatus)
		if !ok {
			return mcp.NewToolResultError("unexpected response type for charging status"), nil
		}

		bitmask := statusPayload.GetChargingStatus.Status
		var activePorts []uint32
		for i := 0; i < maxDevicePorts; i++ {
			if bitmask&(1<<uint(i)) != 0 {
				activePorts = append(activePorts, uint32(i))
			}
		}

		if len(activePorts) == 0 {
			glog.Infof("get_port_pd_status: psn=%d no active ports", psn)
			data, _ := json.Marshal(map[string]any{"ports": []any{}})
			return mcp.NewToolResultText(string(data)), nil
		}

		glog.Infof("get_port_pd_status: psn=%d active_ports=%v", psn, activePorts)

		// Step 2: fan out PD status requests in parallel
		type pdResult struct {
			port uint32
			data map[string]any
			err  error
		}

		results := make([]pdResult, len(activePorts))
		var wg sync.WaitGroup
		for i, port := range activePorts {
			wg.Add(1)
			go func(idx int, p uint32) {
				defer wg.Done()
				resp, err := mqtt.SendCommand(ctx, psn, ServiceGetPortPDStatus, BuildGetPortPDStatus(p))
				if err != nil {
					results[idx] = pdResult{port: p, err: err}
					return
				}
				if resp.Status != pb.CommandStatus_SUCCESS {
					results[idx] = pdResult{port: p, err: fmt.Errorf("device returned status: %s", resp.Status)}
					return
				}
				pdPayload, ok := resp.Payload.(*pb.CommandResponse_GetPortPdStatus)
				if !ok {
					results[idx] = pdResult{port: p, err: fmt.Errorf("unexpected response type")}
					return
				}
				pd := pdPayload.GetPortPdStatus.PdStatus
				entry := map[string]any{
					"port":                               p + 1, // 1-indexed for display
					"battery_vid":                        pd.GetBatteryVid(),
					"battery_pid":                        pd.GetBatteryPid(),
					"battery_design_capacity":            pd.GetBatteryDesignCapacity(),
					"battery_last_full_charge_capacity":  pd.GetBatteryLastFullChargeCapacity(),
					"battery_present_capacity":           pd.GetBatteryPresentCapacity(),
					"battery_invalid":                    pd.GetBatteryInvalid(),
					"battery_present":                    pd.GetBatteryPresent(),
					"battery_status":                     pd.GetBatteryStatus(),
					"has_battery":                        pd.GetHasBattery(),
					"cable_is_active":                    pd.GetCableIsActive(),
					"cable_termination_type":             pd.GetCableTerminationType(),
					"cable_epr_mode_capable":             pd.GetCableEprModeCapable(),
					"cable_active_phy_type":              pd.GetCableActivePhyType(),
					"cable_latency":                      decodeCableLatency(pd.GetCableLatency()),
					"cable_max_vbus_voltage":             decodeCableMaxVbusVoltage(pd.GetCableMaxVbusVoltage()),
					"cable_max_vbus_current":             decodeCableMaxVbusCurrent(pd.GetCableMaxVbusCurrent()),
					"cable_usb_highest_speed":            decodeCableUSBHighestSpeed(pd.GetCableUsbHighestSpeed()),
					"cable_active_element":               decodeCableActiveElement(pd.GetCableActiveElement()),
					"cable_active_usb4":                  pd.GetCableActiveUsb4(),
					"cable_active_usb2p0":                pd.GetCableActiveUsb2P0(),
					"cable_active_usb3p2":                pd.GetCableActiveUsb3P2(),
					"cable_active_usb_lanes":             pd.GetCableActiveUsbLanes(),
					"cable_active_optically_isolated":    pd.GetCableActiveOpticallyIsolated(),
					"cable_active_usb4_asym":             pd.GetCableActiveUsb4Asym(),
					"cable_active_usb_gen":               pd.GetCableActiveUsbGen(),
					"cable_vid":                          pd.GetCableVid(),
					"cable_pid":                          pd.GetCablePid(),
					"cable_xid":                          pd.GetCableXid(),
					"has_emarker":                        pd.GetHasEmarker(),
					"manufacturer_vid":                   pd.GetManufacturerVid(),
					"manufacturer_pid":                   pd.GetManufacturerPid(),
					"bcd_device":                         pd.GetBcdDevice(),
					"operating_current":                  pd.GetOperatingCurrent(),
					"operating_voltage":                  pd.GetOperatingVoltage(),
					"pd_revision":                        decodePDRevision(pd.GetPdRevision()),
					"pps_charging_supported":             pd.GetPpsChargingSupported(),
					"dual_role_power":                    pd.GetDualRolePower(),
					"request_epr_mode_capable":           pd.GetRequestEprModeCapable(),
					"request_pdo_id":                     pd.GetRequestPdoId(),
					"request_usb_communications_capable": pd.GetRequestUsbCommunicationsCapable(),
					"request_capability_mismatch":        pd.GetRequestCapabilityMismatch(),
					"request_ppsavs":                     pd.GetRequestPpsavs(),
					"sink_capabilities":                  pd.GetSinkCapabilities(),
					"sink_cap_pdo_count":                 pd.GetSinkCapPdoCount(),
					"sink_minimum_pdp":                   pd.GetSinkMinimumPdp(),
					"sink_operational_pdp":               pd.GetSinkOperationalPdp(),
					"sink_maximum_pdp":                   pd.GetSinkMaximumPdp(),
					"status_temperature":                 tempRange(pd.GetStatusTemperature()),
				}
				if dev, ok := lookupDevice(pd.GetManufacturerVid(), pd.GetManufacturerPid()); ok {
					delete(entry, "manufacturer_vid")
					delete(entry, "manufacturer_pid")
					entry["device_brand_en"] = dev.BrandEN
					entry["device_brand_zh"] = dev.BrandZH
					entry["device_name_en"] = dev.NameEN
					entry["device_name_zh"] = dev.NameZH
				}
				if cable, ok := lookupCable(pd.GetCableVid(), pd.GetCablePid()); ok {
					delete(entry, "cable_vid")
					delete(entry, "cable_pid")
					entry["cable_brand_en"] = cable.BrandEN
					entry["cable_brand_zh"] = cable.BrandZH
					entry["cable_name_en"] = cable.NameEN
					entry["cable_name_zh"] = cable.NameZH
				}
				results[idx] = pdResult{port: p, data: entry}
			}(i, port)
		}
		wg.Wait()

		// Step 3: collect results
		var portStatuses []any
		for _, r := range results {
			if r.err != nil {
				glog.Warningf("get_port_pd_status: psn=%d port=%d err=%v", psn, r.port, r.err)
				portStatuses = append(portStatuses, map[string]any{
					"port":  r.port + 1,
					"error": r.err.Error(),
				})
			} else {
				portStatuses = append(portStatuses, r.data)
			}
		}

		data, _ := json.Marshal(map[string]any{"ports": portStatuses})
		glog.Infof("get_port_pd_status: ok psn=%d ports=%d", psn, len(portStatuses))
		return mcp.NewToolResultText(string(data)), nil
	}
}

// parsePortsArg extracts []uint32 from the "ports" argument.
func parsePortsArg(request mcp.CallToolRequest) ([]uint32, error) {
	args := request.GetArguments()
	rawPorts, ok := args["ports"]
	if !ok {
		return nil, fmt.Errorf("ports is required")
	}

	portsSlice, ok := rawPorts.([]any)
	if !ok {
		return nil, fmt.Errorf("ports must be an array")
	}

	ports := make([]uint32, len(portsSlice))
	for i, v := range portsSlice {
		switch n := v.(type) {
		case float64:
			// JSON numbers are decoded as float64 by default; require integer semantics.
			if n != float64(int64(n)) || n < minUserPort || n > maxDevicePorts {
				return nil, fmt.Errorf("invalid port number at index %d", i)
			}
			ports[i] = uint32(int64(n))
		case json.Number:
			val, err := n.Int64()
			if err != nil || val < minUserPort || val > maxDevicePorts {
				return nil, fmt.Errorf("invalid port number at index %d", i)
			}
			ports[i] = uint32(val)
		default:
			return nil, fmt.Errorf("invalid port value type at index %d", i)
		}
	}

	return ports, nil
}

func productFamilyForPSN(ctx context.Context, pool *pgxpool.Pool, psn uint64) string {
	if pool == nil {
		return ""
	}

	var productFamily string
	err := pool.QueryRow(ctx,
		`SELECT product_family FROM device_information WHERE psn = $1`, psn,
	).Scan(&productFamily)
	if err != nil {
		glog.Warningf("display config: product family lookup failed: psn=%d err=%v", psn, err)
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(productFamily), "-", ""))
}

func displayBrightnessLevelsForProductFamily(productFamily string) ([]displayBrightnessLevel, bool) {
	switch productFamily {
	case "cp02":
		return cp02DisplayBrightnessLevels, true
	case "cp02s":
		return cp02sDisplayBrightnessLevels, true
	default:
		return nil, false
	}
}

func displayBrightnessLevelByName(levels []displayBrightnessLevel, name string) (displayBrightnessLevel, bool) {
	normalized := strings.TrimSpace(name)
	for _, level := range levels {
		if normalized == level.Name {
			return level, true
		}
	}
	return displayBrightnessLevel{}, false
}

func displayBrightnessLevelByIntensity(levels []displayBrightnessLevel, intensity uint32) displayBrightnessLevel {
	for _, level := range levels {
		for _, alias := range level.Aliases {
			if intensity == alias {
				return level
			}
		}
	}

	var best displayBrightnessLevel
	var bestDistance uint32
	for i, level := range levels {
		var distance uint32
		if intensity > level.Intensity {
			distance = intensity - level.Intensity
		} else {
			distance = level.Intensity - intensity
		}
		if i == 0 || distance < bestDistance {
			best = level
			bestDistance = distance
		}
	}
	return best
}

func cp02sDisplayModeByName(name string) (cp02sDisplayModeOption, bool) {
	normalized := strings.TrimSpace(name)
	for _, option := range cp02sDisplayModeOptions {
		if normalized == option.Name {
			return option, true
		}
	}
	return cp02sDisplayModeOption{}, false
}

func cp02sDisplayModeByDuration(duration uint32) cp02sDisplayModeOption {
	for _, option := range cp02sDisplayModeOptions {
		if duration == option.Duration {
			return option
		}
	}
	return cp02sDisplayModeOptions[0]
}

func cp02sIdleDisplayByName(name string) (cp02sIdleDisplayOption, bool) {
	normalized := strings.TrimSpace(name)
	for _, option := range cp02sIdleDisplayOptions {
		if normalized == option.Name {
			return option, true
		}
	}
	return cp02sIdleDisplayOption{}, false
}

func cp02sIdleDisplayByAnimation(animation pb.IdleAnimationType) cp02sIdleDisplayOption {
	for _, option := range cp02sIdleDisplayOptions {
		if animation == option.Animation {
			return option
		}
	}
	return cp02sIdleDisplayOptions[0]
}

func hourlyChimeText(enabled bool) string {
	if enabled {
		return "开启"
	}
	return "关闭"
}

func waitForDisplayConfig(ctx context.Context, telemetry *TelemetryManager, psn uint64) (*pb.DisplayConfig, error) {
	return waitForDisplayConfigUpdatedAfter(ctx, telemetry, psn, time.Now().Add(-telemetryStaleness))
}

func waitForFreshDisplayConfig(ctx context.Context, telemetry *TelemetryManager, psn uint64) (*pb.DisplayConfig, error) {
	minUpdated := time.Now()
	telemetry.KickDevice(ctx, psn)
	telemetry.kick(ctx, psn)
	return waitForDisplayConfigUpdatedAfter(ctx, telemetry, psn, minUpdated)
}

func waitForDisplayConfigUpdatedAfter(ctx context.Context, telemetry *TelemetryManager, psn uint64, minUpdated time.Time) (*pb.DisplayConfig, error) {
	streamData, updated := telemetry.LatestDevice(psn)
	if streamData != nil && updated.After(minUpdated) && time.Since(updated) < telemetryStaleness {
		if dc := streamData.GetNewDisplayConfig(); dc != nil {
			return dc, nil
		}
	}

	glog.Infof("display config: psn=%d cache miss/stale, triggering telemetry", psn)
	telemetry.KickDevice(ctx, psn)

	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cached, updated := telemetry.LatestDevice(psn)
			if cached == nil || !updated.After(minUpdated) || time.Since(updated) >= telemetryStaleness {
				continue
			}
			if dc := cached.GetNewDisplayConfig(); dc != nil {
				return dc, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("display config unavailable — device may be offline")
		case <-ctx.Done():
			return nil, fmt.Errorf("request cancelled")
		}
	}
}

func displayIntensityFromDevice(ctx context.Context, mqtt *MQTTClient, psn uint64) (uint32, error) {
	resp, err := mqtt.SendCommand(ctx, psn, ServiceGetDisplayIntensity, BuildGetDisplayIntensity())
	if err != nil {
		return 0, fmt.Errorf("get display intensity: %w", err)
	}
	if resp.Status != pb.CommandStatus_SUCCESS {
		return 0, fmt.Errorf("device returned status: %s", resp.Status)
	}
	payload, ok := resp.Payload.(*pb.CommandResponse_GetDisplayIntensity)
	if !ok || payload.GetDisplayIntensity == nil {
		return 0, fmt.Errorf("device did not return display intensity")
	}
	return payload.GetDisplayIntensity.GetIntensity(), nil
}

func currentDisplayConfigForUpdate(ctx context.Context, mqtt *MQTTClient, pool *pgxpool.Pool, telemetry *TelemetryManager, psn uint64) (*pb.DisplayConfig, error) {
	productFamily := productFamilyForPSN(ctx, pool, psn)
	if productFamily != "cp02s" {
		return nil, fmt.Errorf("status display settings are only supported for CP02S")
	}

	intensity, err := displayIntensityFromDevice(ctx, mqtt, psn)
	if err != nil {
		return nil, err
	}

	current, err := waitForFreshDisplayConfig(ctx, telemetry, psn)
	if err != nil {
		return nil, err
	}
	config := *current
	config.Intensity = intensity
	config.Mode = pb.DisplayMode_POWER_METER
	return &config, nil
}

func applyDisplayConfig(ctx context.Context, mqtt *MQTTClient, psn uint64, config *pb.DisplayConfig) error {
	resp, err := mqtt.SendCommand(ctx, psn, ServiceSetDisplayConfigLegacy, BuildSetDisplayConfigLegacy(config))
	if err != nil {
		return fmt.Errorf("set display config: %w", err)
	}
	if resp.Status != pb.CommandStatus_SUCCESS {
		return fmt.Errorf("device returned status: %s", resp.Status)
	}
	return nil
}

func applyDisplayConfigWithNewAttempt(ctx context.Context, mqtt *MQTTClient, psn uint64, config *pb.DisplayConfig) error {
	resp, err := mqtt.SendCommand(ctx, psn, ServiceSetDisplayConfig, BuildSetDisplayConfig(config))
	if err != nil {
		glog.Warningf("set_display_config: new payload failed, trying legacy: psn=%d err=%v", psn, err)
	} else if resp.Status != pb.CommandStatus_SUCCESS {
		glog.Warningf("set_display_config: new payload rejected, trying legacy: psn=%d status=%s", psn, resp.Status)
	} else {
		return nil
	}
	return applyDisplayConfig(ctx, mqtt, psn, config)
}

// Tool 14: get_display_config
func makeGetDisplayConfigHandler(mqtt *MQTTClient, pool *pgxpool.Pool, telemetry *TelemetryManager) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		productFamily := productFamilyForPSN(ctx, pool, psn)
		levels, ok := displayBrightnessLevelsForProductFamily(productFamily)
		if !ok {
			return mcp.NewToolResultError("display brightness is not supported for this device"), nil
		}

		intensity, err := displayIntensityFromDevice(ctx, mqtt, psn)
		if err != nil {
			glog.Warningf("get_display_config: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		brightnessLevel := displayBrightnessLevelByIntensity(levels, intensity)

		result := map[string]any{
			"level": brightnessLevel.Name,
		}

		if productFamily == "cp02s" {
			if dc, err := waitForDisplayConfig(ctx, telemetry, psn); err == nil {
				result["display_mode"] = cp02sDisplayModeByDuration(dc.GetChargingAnimationDuration()).Name
				result["idle_display"] = cp02sIdleDisplayByAnimation(dc.GetIdleAnimation()).Name
				result["hourly_chime"] = dc.GetHourlyChime()
				result["hourly_chime_text"] = hourlyChimeText(dc.GetHourlyChime())
			} else {
				glog.Warningf("get_display_config: optional telemetry unavailable: psn=%d err=%v", psn, err)
			}
		}

		data, _ := json.Marshal(result)

		glog.Infof("get_display_config: ok psn=%d level=%s intensity=%d", psn, brightnessLevel.Name, intensity)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 15: set_display_intensity
func makeSetDisplayIntensityHandler(mqtt *MQTTClient, pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		levelName, err := request.RequireString("level")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		productFamily := productFamilyForPSN(ctx, pool, psn)
		levels, ok := displayBrightnessLevelsForProductFamily(productFamily)
		if !ok {
			return mcp.NewToolResultError("display brightness is not supported for this device"), nil
		}
		level, ok := displayBrightnessLevelByName(levels, levelName)
		if !ok {
			return mcp.NewToolResultError("level must be one of: 关, 低, 中, 高"), nil
		}
		intensity := level.Intensity

		glog.Infof("set_display_intensity: psn=%d level=%s intensity=%d", psn, level.Name, intensity)

		resp, err := mqtt.SendCommand(ctx, psn, ServiceSetDisplayIntensity, BuildSetDisplayIntensity(intensity))
		if err != nil {
			glog.Warningf("set_display_intensity: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(fmt.Sprintf("set display intensity: %v", err)), nil
		}
		if resp.Status != pb.CommandStatus_SUCCESS {
			return mcp.NewToolResultError(fmt.Sprintf("device returned status: %s", resp.Status)), nil
		}

		data, _ := json.Marshal(map[string]any{
			"level": level.Name,
		})
		glog.Infof("set_display_intensity: ok psn=%d level=%s intensity=%d", psn, level.Name, intensity)
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 16: set_status_display_mode
func makeSetStatusDisplayModeHandler(mqtt *MQTTClient, pool *pgxpool.Pool, telemetry *TelemetryManager) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		modeName, err := request.RequireString("mode")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		mode, ok := cp02sDisplayModeByName(modeName)
		if !ok {
			return mcp.NewToolResultError("mode must be one of: 待机动画优先, 功率显示优先"), nil
		}

		config, err := currentDisplayConfigForUpdate(ctx, mqtt, pool, telemetry, psn)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		config.ChargingAnimationDuration = mode.Duration

		glog.Infof("set_status_display_mode: psn=%d mode=%s duration=%d", psn, mode.Name, mode.Duration)
		if err := applyDisplayConfigWithNewAttempt(ctx, mqtt, psn, config); err != nil {
			glog.Warningf("set_status_display_mode: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(err.Error()), nil
		}

		data, _ := json.Marshal(map[string]any{
			"display_mode": mode.Name,
		})
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 17: set_idle_display
func makeSetIdleDisplayHandler(mqtt *MQTTClient, pool *pgxpool.Pool, telemetry *TelemetryManager) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		idleDisplayName, err := request.RequireString("idle_display")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		idleDisplay, ok := cp02sIdleDisplayByName(idleDisplayName)
		if !ok {
			return mcp.NewToolResultError("idle_display must be one of: 流星, 落花, 康威的生命游戏, 时间"), nil
		}

		config, err := currentDisplayConfigForUpdate(ctx, mqtt, pool, telemetry, psn)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		config.IdleAnimation = idleDisplay.Animation

		glog.Infof("set_idle_display: psn=%d idle_display=%s animation=%s", psn, idleDisplay.Name, idleDisplay.Animation)
		if err := applyDisplayConfigWithNewAttempt(ctx, mqtt, psn, config); err != nil {
			glog.Warningf("set_idle_display: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(err.Error()), nil
		}

		data, _ := json.Marshal(map[string]any{
			"idle_display": idleDisplay.Name,
		})
		return mcp.NewToolResultText(string(data)), nil
	}
}

// Tool 18: set_hourly_chime
func makeSetHourlyChimeHandler(mqtt *MQTTClient, pool *pgxpool.Pool, telemetry *TelemetryManager) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		psn, err := psnFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		enabled, err := request.RequireBool("enabled")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		config, err := currentDisplayConfigForUpdate(ctx, mqtt, pool, telemetry, psn)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		config.HourlyChime = proto.Bool(enabled)

		glog.Infof("set_hourly_chime: psn=%d enabled=%v", psn, enabled)
		if err := applyDisplayConfigWithNewAttempt(ctx, mqtt, psn, config); err != nil {
			glog.Warningf("set_hourly_chime: failed: psn=%d err=%v", psn, err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		current, err := waitForFreshDisplayConfig(ctx, telemetry, psn)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if current.GetHourlyChime() != enabled {
			return mcp.NewToolResultError(fmt.Sprintf("hourly chime remains %s; device did not apply %s", hourlyChimeText(current.GetHourlyChime()), hourlyChimeText(enabled))), nil
		}

		idleDisplay := cp02sIdleDisplayByAnimation(current.GetIdleAnimation())
		data, _ := json.Marshal(map[string]any{
			"hourly_chime":      current.GetHourlyChime(),
			"hourly_chime_text": hourlyChimeText(current.GetHourlyChime()),
			"idle_display":      idleDisplay.Name,
		})
		return mcp.NewToolResultText(string(data)), nil
	}
}
