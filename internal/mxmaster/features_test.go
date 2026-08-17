package mxmaster

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/atremb/logitechd/internal/config"
	"github.com/atremb/logitechd/internal/hidpp"
	"github.com/atremb/logitechd/internal/receiver"
)

type fakeCall struct {
	index  byte
	fn     byte
	params []byte
}

type fakeFeatureDevice struct {
	features  map[uint16]hidpp.FeatureInfo
	responses map[byte][][]byte
	callErrs  map[byte][]error
	calls     []fakeCall
}

func (f *fakeFeatureDevice) LookupFeature(_ context.Context, id uint16) (hidpp.FeatureInfo, error) {
	if info, ok := f.features[id]; ok {
		return info, nil
	}
	return hidpp.FeatureInfo{}, &hidpp.UnsupportedError{Operation: "test feature", Detail: "not present"}
}

func (f *fakeFeatureDevice) Call(_ context.Context, index, function byte, params []byte) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{index: index, fn: function, params: append([]byte(nil), params...)})
	if queue := f.callErrs[function]; len(queue) > 0 {
		err := queue[0]
		f.callErrs[function] = queue[1:]
		if err != nil {
			return nil, err
		}
	}
	queue := f.responses[function]
	if len(queue) == 0 {
		return nil, errors.New("test fake: no response")
	}
	result := append([]byte(nil), queue[0]...)
	f.responses[function] = queue[1:]
	return result, nil
}

func featureDevice(id uint16, index byte) *fakeFeatureDevice {
	return &fakeFeatureDevice{
		features:  map[uint16]hidpp.FeatureInfo{id: {ID: id, Index: index}},
		responses: make(map[byte][][]byte),
		callErrs:  make(map[byte][]error),
	}
}

func TestApplySmartShiftIgnoresUnsupportedTorque(t *testing.T) {
	fake := featureDevice(FeatureSmartShiftV2, 4)
	fake.responses[0x10] = [][]byte{{1, 20, 70}, {1, 20, 70}, {1, 20, 70}}
	fake.responses[0x00] = [][]byte{{0}, {0}, {0}}
	fake.responses[0x20] = [][]byte{{}}
	client, err := NewSmartShift(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	enabled, threshold, torque := true, 100, 70
	configurator := &Configurator{
		settings: config.Config{SmartShift: &config.SmartShiftConfig{
			Enabled:   &enabled,
			Threshold: &threshold,
			Torque:    &torque,
		}},
		features: &FeatureSet{SmartShift: client},
	}

	if err := configurator.applySmartShift(context.Background()); err != nil {
		t.Fatalf("applySmartShift() error = %v", err)
	}

	var statusSets []fakeCall
	for _, call := range fake.calls {
		if call.fn == 0x20 {
			statusSets = append(statusSets, call)
		}
	}
	if len(statusSets) != 1 {
		t.Fatalf("SmartShift set calls = %d, want 1", len(statusSets))
	}
	if got := statusSets[0].params; !reflect.DeepEqual(got, []byte{2, 100, 70}) {
		t.Fatalf("SmartShift status set params = %v, want [2 100 70]", got)
	}
}

func TestApplySmartShiftPropagatesTorqueFailures(t *testing.T) {
	tests := []struct {
		name    string
		failure error
		target  error
	}{
		{
			name:    "transport",
			failure: &hidpp.ClosedTransportError{Cause: errors.New("link reset")},
			target:  hidpp.ErrClosedTransport,
		},
		{
			name:    "protocol",
			failure: &hidpp.ProtocolError{Code: 0x05},
			target:  hidpp.ErrProtocol,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := featureDevice(FeatureSmartShiftV2, 4)
			fake.responses[0x10] = [][]byte{{1, 20, 70}, {1, 20, 70}}
			fake.responses[0x00] = [][]byte{{0}, {0}}
			fake.responses[0x20] = [][]byte{{}}
			fake.callErrs[0x10] = []error{nil, nil, test.failure}
			client, err := NewSmartShift(context.Background(), fake)
			if err != nil {
				t.Fatal(err)
			}
			enabled, threshold, torque := true, 100, 70
			configurator := &Configurator{
				settings: config.Config{SmartShift: &config.SmartShiftConfig{
					Enabled:   &enabled,
					Threshold: &threshold,
					Torque:    &torque,
				}},
				features: &FeatureSet{SmartShift: client},
			}

			err = configurator.applySmartShift(context.Background())
			if err == nil || !errors.Is(err, test.target) {
				t.Fatalf("applySmartShift() error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestSmartShiftVersionFallbackAndStatus(t *testing.T) {
	v1 := featureDevice(FeatureSmartShift, 3)
	v1.responses[0x00] = [][]byte{{1, 20}}
	client, err := NewSmartShift(context.Background(), v1)
	if err != nil {
		t.Fatal(err)
	}
	if client.Version() != 1 || client.FeatureIndex() != 3 {
		t.Fatalf("client version/index = %d/%d", client.Version(), client.FeatureIndex())
	}
	status, err := client.GetStatus(context.Background())
	if err != nil || status.Mode != SmartShiftModeFreeSpin || status.Threshold != 20 {
		t.Fatalf("status = %+v, error=%v", status, err)
	}

	v2 := featureDevice(FeatureSmartShiftV2, 4)
	v2.responses[0x10] = [][]byte{{2, 25, 70}}
	v2.responses[0x00] = [][]byte{{1}}
	client, err = NewSmartShift(context.Background(), v2)
	if err != nil {
		t.Fatal(err)
	}
	status, err = client.GetStatus(context.Background())
	if err != nil || status.Mode != SmartShiftModeRatchet || !status.TorqueSupported || status.Torque != 70 {
		t.Fatalf("enhanced status = %+v, error=%v", status, err)
	}

	invalid := featureDevice(FeatureSmartShift, 3)
	invalid.responses[0x00] = [][]byte{{0, 25}}
	client, err = NewSmartShift(context.Background(), invalid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetStatus(context.Background()); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("zero status mode error = %v, want malformed response", err)
	}
}

func TestSmartShiftExplicitModeWireFormats(t *testing.T) {
	tests := []struct {
		name       string
		feature    uint16
		setFn      byte
		wantTorque bool
	}{
		{name: "v1", feature: FeatureSmartShift, setFn: 0x10},
		{name: "v2", feature: FeatureSmartShiftV2, setFn: 0x20, wantTorque: true},
	}
	modes := []SmartShiftMode{SmartShiftModeFreeSpin, SmartShiftModeRatchet}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := featureDevice(test.feature, 4)
			if test.wantTorque {
				fake.responses[0x10] = [][]byte{{2, 25, 70}, {2, 25, 70}}
				fake.responses[0x00] = [][]byte{{1}, {1}}
			}
			fake.responses[test.setFn] = [][]byte{{}, {}}
			client, err := NewSmartShift(context.Background(), fake)
			if err != nil {
				t.Fatal(err)
			}

			for _, mode := range modes {
				if err := client.SetStatus(context.Background(), mode, 100); err != nil {
					t.Fatalf("SetStatus(%d) error = %v", mode, err)
				}
				want := []byte{byte(mode), 100}
				if test.wantTorque {
					want = append(want, 70)
				}
				if got := fake.calls[len(fake.calls)-1]; got.fn != test.setFn || !reflect.DeepEqual(got.params, want) {
					t.Fatalf("SetStatus(%d) call = %+v, want function %#x params %v", mode, got, test.setFn, want)
				}
			}
			if err := client.SetStatus(context.Background(), SmartShiftMode(3), 100); err == nil {
				t.Fatal("unknown mode was accepted")
			}
			if err := client.SetStatus(context.Background(), SmartShiftMode(0), 100); err == nil {
				t.Fatal("set-only preserve mode was accepted as a concrete status")
			}
			if err := client.SetStatus(context.Background(), SmartShiftModeRatchet, 0); err == nil {
				t.Fatal("zero threshold was accepted")
			}
		})
	}
}

func TestApplySmartShiftFixedRatchetAndTorque(t *testing.T) {
	fake := featureDevice(FeatureSmartShiftV2, 4)
	fake.responses[0x10] = [][]byte{{1, 20, 70}, {1, 20, 70}, {2, 255, 70}}
	fake.responses[0x00] = [][]byte{{1}, {1}, {1}}
	fake.responses[0x20] = [][]byte{{}, {}}
	client, err := NewSmartShift(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	enabled, threshold, torque := true, 255, 80
	configurator := &Configurator{
		settings: config.Config{SmartShift: &config.SmartShiftConfig{
			Enabled:   &enabled,
			Threshold: &threshold,
			Torque:    &torque,
		}},
		features: &FeatureSet{SmartShift: client},
	}

	if err := configurator.applySmartShift(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sets [][]byte
	for _, call := range fake.calls {
		if call.fn == 0x20 {
			sets = append(sets, call.params)
		}
	}
	want := [][]byte{{2, 255, 70}, {0, 0, 80}}
	if !reflect.DeepEqual(sets, want) {
		t.Fatalf("SmartShift sets = %v, want %v", sets, want)
	}
}

func TestApplySmartShiftOmittedEnabledPreservesCurrentMode(t *testing.T) {
	fake := featureDevice(FeatureSmartShift, 3)
	fake.responses[0x00] = [][]byte{{2, 25}}
	fake.responses[0x10] = [][]byte{{}}
	client, err := NewSmartShift(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	threshold := 100
	configurator := &Configurator{
		settings: config.Config{SmartShift: &config.SmartShiftConfig{Threshold: &threshold}},
		features: &FeatureSet{SmartShift: client},
	}

	if err := configurator.applySmartShift(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls[len(fake.calls)-1]; got.fn != 0x10 || !reflect.DeepEqual(got.params, []byte{0, 100}) {
		t.Fatalf("threshold set = %+v, want preserved mode and threshold 100", got)
	}
}

func TestApplySmartShiftEnabledMappings(t *testing.T) {
	tests := []struct {
		name             string
		enabled          bool
		threshold        *int
		currentThreshold byte
		want             []byte
	}{
		{name: "disabled", enabled: false, currentThreshold: 25, want: []byte{1, 255}},
		{name: "enabled automatic", enabled: true, threshold: intPtr(100), currentThreshold: 25, want: []byte{2, 100}},
		{name: "enabled fixed ratchet", enabled: true, threshold: intPtr(255), currentThreshold: 25, want: []byte{2, 255}},
		{name: "enabled reuses fixed threshold", enabled: true, currentThreshold: 255, want: []byte{2, 255}},
	}
	versions := []struct {
		name     string
		feature  uint16
		statusFn byte
		setFn    byte
		v2       bool
	}{
		{name: "v1", feature: FeatureSmartShift, statusFn: 0x00, setFn: 0x10},
		{name: "v2", feature: FeatureSmartShiftV2, statusFn: 0x10, setFn: 0x20, v2: true},
	}

	for _, version := range versions {
		for _, test := range tests {
			t.Run(version.name+"/"+test.name, func(t *testing.T) {
				fake := featureDevice(version.feature, 3)
				status := []byte{2, test.currentThreshold}
				if version.v2 {
					status = append(status, 70)
					fake.responses[version.statusFn] = [][]byte{status, status}
					fake.responses[0x00] = [][]byte{{1}, {1}}
				} else {
					fake.responses[version.statusFn] = [][]byte{status}
				}
				fake.responses[version.setFn] = [][]byte{{}}
				client, err := NewSmartShift(context.Background(), fake)
				if err != nil {
					t.Fatal(err)
				}
				configurator := &Configurator{
					settings: config.Config{SmartShift: &config.SmartShiftConfig{
						Enabled:   &test.enabled,
						Threshold: test.threshold,
					}},
					features: &FeatureSet{SmartShift: client},
				}

				if err := configurator.applySmartShift(context.Background()); err != nil {
					t.Fatal(err)
				}
				want := append([]byte(nil), test.want...)
				if version.v2 {
					want = append(want, 70)
				}
				if got := fake.calls[len(fake.calls)-1]; got.fn != version.setFn || !reflect.DeepEqual(got.params, want) {
					t.Fatalf("SmartShift set = %+v, want function %#x params %v", got, version.setFn, want)
				}
			})
		}
	}
}

func intPtr(value int) *int { return &value }

func TestSmartShiftCleanupRestoresExactOriginalState(t *testing.T) {
	fake := featureDevice(FeatureSmartShiftV2, 4)
	fake.responses[0x10] = [][]byte{
		{1, 42, 65},
		{1, 42, 65},
		{2, 255, 65},
		{2, 255, 80},
		{1, 42, 80},
	}
	fake.responses[0x00] = [][]byte{{1}, {1}, {1}, {1}, {1}}
	fake.responses[0x20] = [][]byte{{}, {}, {}, {}}
	client, err := NewSmartShift(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	enabled, threshold, torque := true, 255, 80
	configurator := &Configurator{
		settings: config.Config{
			SmartShift: &config.SmartShiftConfig{Enabled: &enabled, Threshold: &threshold, Torque: &torque},
		},
		features: &FeatureSet{SmartShift: client},
		events:   make(chan InputEvent),
	}

	if err := configurator.applySmartShift(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := configurator.Close(); err != nil {
		t.Fatal(err)
	}
	var sets [][]byte
	for _, call := range fake.calls {
		if call.fn == 0x20 {
			sets = append(sets, call.params)
		}
	}
	want := [][]byte{
		{2, 255, 65},
		{0, 0, 80},
		{1, 42, 80},
		{0, 0, 65},
	}
	if !reflect.DeepEqual(sets, want) {
		t.Fatalf("SmartShift sets = %v, want %v", sets, want)
	}
}

func TestHiResWireAndEventDecoding(t *testing.T) {
	fake := featureDevice(FeatureHiResWheel, 7)
	fake.responses[0x00] = [][]byte{{8}}
	fake.responses[0x10] = [][]byte{{0x05}}
	fake.responses[0x20] = [][]byte{{0x07}}
	client, err := NewHiResWheel(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := client.GetCapabilities(context.Background())
	if err != nil || capability != 8 {
		t.Fatalf("capability = %d, error=%v", capability, err)
	}
	mode, err := client.GetMode(context.Background())
	if err != nil || !mode.UseHIDPP || mode.HighResolution || !mode.Invert {
		t.Fatalf("mode = %+v, error=%v", mode, err)
	}
	if err := client.SetMode(context.Background(), WheelMode{UseHIDPP: true, HighResolution: true, Invert: true}); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls[len(fake.calls)-1]; got.fn != 0x20 || !reflect.DeepEqual(got.params, []byte{0x07}) {
		t.Fatalf("mode set = %+v", got)
	}
	event, err := DecodeWheelEvent(hidpp.Report{Function: 0, Parameters: []byte{0x15, 0xff, 0xf6}})
	if err != nil || !event.HighResolution || event.Periods != 5 || event.Delta != -10 {
		t.Fatalf("wheel event = %+v, error=%v", event, err)
	}
}

func TestDPIListRangeRoundingAndWireFormats(t *testing.T) {
	fake := featureDevice(FeatureAdjustableDPI, 9)
	fake.responses[0x10] = [][]byte{{0, 0x01, 0xf4, 0x03, 0xe8, 0, 0}}
	fake.responses[0x20] = [][]byte{{0, 0x06, 0x40, 0x03, 0xe8}}
	fake.responses[0x30] = [][]byte{{0}}
	client, err := NewAdjustableDPI(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.Supported(context.Background(), 0)
	if err != nil || !reflect.DeepEqual(values, []uint16{500, 1000}) {
		t.Fatalf("DPI values = %v, error=%v", values, err)
	}
	rangeValues, err := decodeDPIList([]byte{0x01, 0xf4, 0xe0, 0x32, 0x03, 0xe8, 0, 0})
	if err != nil || len(rangeValues) != 11 || rangeValues[1] != 550 || rangeValues[10] != 1000 {
		t.Fatalf("DPI range = %v, error=%v", rangeValues, err)
	}
	nearest, err := NearestDPI([]uint16{500, 1000, 1500}, 1200)
	if err != nil || nearest != 1000 {
		t.Fatalf("nearest DPI = %d, error=%v", nearest, err)
	}
	current, err := client.Current(context.Background(), 0)
	if err != nil || current.Current != 1600 || current.Default != 1000 {
		t.Fatalf("current DPI = %+v, error=%v", current, err)
	}
	if err := client.Set(context.Background(), 0, 1600); err != nil {
		t.Fatal(err)
	}
	last := fake.calls[len(fake.calls)-1]
	if last.fn != 0x30 || !reflect.DeepEqual(last.params, []byte{0, 0x06, 0x40}) {
		t.Fatalf("DPI set = %+v", last)
	}
}

func TestControlsVersionInfoDiversionAndEvents(t *testing.T) {
	fake := featureDevice(FeatureControlsV4, 11)
	fake.responses[0x00] = [][]byte{{3}}
	fake.responses[0x10] = [][]byte{{0, 0x53, 0, 0x9d, 0x31, 4, 2, 3, 1}}
	fake.responses[0x20] = [][]byte{{0, 0x53, 0x11, 0, 0x53}}
	fake.responses[0x30] = [][]byte{{0, 0x53, 0x03, 0, 0}}
	client, err := NewControls(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if client.Version() != 4 {
		t.Fatalf("controls version = %d", client.Version())
	}
	count, err := client.Count(context.Background())
	if err != nil || count != 3 {
		t.Fatalf("control count = %d, error=%v", count, err)
	}
	info, err := client.Info(context.Background(), 2)
	if err != nil || info.CID != 0x53 || info.Task != 0x9d || info.Flags != 0x0131 || info.GroupMask != 3 {
		t.Fatalf("control info = %+v, error=%v", info, err)
	}
	reporting, err := client.Reporting(context.Background(), 0x53)
	if err != nil || !reporting.Diverted || !reporting.RawXY || reporting.Remap != 0x53 {
		t.Fatalf("control reporting = %+v, error=%v", reporting, err)
	}
	if _, err := client.SetTemporaryDiversion(context.Background(), 0x53, true); err != nil {
		t.Fatal(err)
	}
	last := fake.calls[len(fake.calls)-1]
	if last.fn != 0x30 || !reflect.DeepEqual(last.params, []byte{0, 0x53, 0x03, 0, 0}) {
		t.Fatalf("control diversion = %+v", last)
	}
	buttons, err := DecodeControlButtonEvent(hidpp.Report{Function: 0, Parameters: []byte{0, 0xc3, 0, 0x53, 0, 0, 0, 0}})
	if err != nil || !reflect.DeepEqual(buttons.ControlIDs, []uint16{0xc3, 0x53}) {
		t.Fatalf("button event = %+v, error=%v", buttons, err)
	}
	raw, err := DecodeRawXYEvent(hidpp.Report{Function: 1, Parameters: []byte{0xff, 0xfe, 0, 3}})
	if err != nil || raw.DX != -2 || raw.DY != 3 {
		t.Fatalf("raw XY = %+v, error=%v", raw, err)
	}
}

func TestThumbWheelAndTruncatedResponses(t *testing.T) {
	fake := featureDevice(FeatureThumbWheel, 12)
	fake.responses[0x00] = [][]byte{{1, 2}}
	fake.responses[0x10] = [][]byte{{1, 0}}
	fake.responses[0x20] = [][]byte{{1, 1}}
	client, err := NewThumbWheel(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.Info(context.Background())
	if err != nil || !reflect.DeepEqual(info, []byte{1, 2}) {
		t.Fatalf("thumb info = %x, error=%v", info, err)
	}
	status, err := client.Status(context.Background())
	if err != nil || !status.Diverted || status.Inverted {
		t.Fatalf("thumb status = %+v, error=%v", status, err)
	}
	if err := client.SetReporting(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls[len(fake.calls)-1]; got.fn != 0x20 || !reflect.DeepEqual(got.params, []byte{1, 1}) {
		t.Fatalf("thumb set = %+v", got)
	}
	event, err := DecodeThumbWheelEvent(hidpp.Report{Function: 0, Parameters: []byte{0xff, 0xf0}})
	if err != nil || event.Delta != -16 {
		t.Fatalf("thumb event = %+v, error=%v", event, err)
	}

	if _, err := DecodeWheelEvent(hidpp.Report{Function: 0, Parameters: []byte{1, 2}}); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("truncated wheel error = %v", err)
	}
	if _, err := DecodeControlButtonEvent(hidpp.Report{Function: 0, Parameters: []byte{0, 1, 0}}); !errors.Is(err, hidpp.ErrMalformedResponse) {
		t.Fatalf("truncated button error = %v", err)
	}
	if _, err := client.Status(context.Background()); err == nil {
		// The fake has no second status response; this checks that callers do
		// not accidentally turn a missing response into a zero status.
		t.Fatal("missing thumb response unexpectedly succeeded")
	}

}

func TestTargetSelectionByNameIndexAndDefault(t *testing.T) {
	children := []receiver.ChildMetadata{
		{WirelessIndex: 1, Name: "Other Mouse"},
		{WirelessIndex: 2, Name: config.DefaultDeviceName},
	}
	selected, err := SelectMetadata(children, config.DeviceConfig{})
	if err != nil || selected.WirelessIndex != 2 {
		t.Fatalf("default selection = %+v, error=%v", selected, err)
	}
	selected, err = SelectMetadata(children, config.DeviceConfig{Index: 1})
	if err != nil || selected.Name != "Other Mouse" {
		t.Fatalf("index selection = %+v, error=%v", selected, err)
	}
	selected, err = SelectMetadata(children, config.DeviceConfig{Name: config.DefaultDeviceName, Index: 2})
	if err != nil || selected.WirelessIndex != 2 {
		t.Fatalf("combined selection = %+v, error=%v", selected, err)
	}
	if _, err := SelectMetadata(children, config.DeviceConfig{Name: config.DefaultDeviceName, Index: 1}); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("mismatched selection error = %v", err)
	}
}
