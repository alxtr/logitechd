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
	queue := f.responses[function]
	if len(queue) == 0 {
		return nil, errors.New("test fake: no response")
	}
	result := append([]byte(nil), queue[0]...)
	f.responses[function] = queue[1:]
	return result, nil
}

func featureDevice(id uint16, index byte) *fakeFeatureDevice {
	return &fakeFeatureDevice{features: map[uint16]hidpp.FeatureInfo{id: {ID: id, Index: index}}, responses: make(map[byte][][]byte)}
}

func TestSmartShiftVersionFallbackAndWireFormats(t *testing.T) {
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
	if err != nil || status.Enabled || status.Threshold != 20 {
		t.Fatalf("status = %+v, error=%v", status, err)
	}

	v2 := featureDevice(FeatureSmartShiftV2, 4)
	v2.responses[0x10] = [][]byte{{0, 25, 70}, {0, 25, 70}}
	v2.responses[0x00] = [][]byte{{1}, {1}}
	v2.responses[0x20] = [][]byte{{0, 30, 80}}
	client, err = NewSmartShift(context.Background(), v2)
	if err != nil {
		t.Fatal(err)
	}
	status, err = client.GetStatus(context.Background())
	if err != nil || !status.Enabled || !status.TorqueSupported || status.Torque != 70 {
		t.Fatalf("enhanced status = %+v, error=%v", status, err)
	}
	if err := client.SetStatus(context.Background(), true, 30); err != nil {
		t.Fatal(err)
	}
	if got := v2.calls[len(v2.calls)-1]; got.fn != 0x20 || !reflect.DeepEqual(got.params, []byte{0, 30, 70}) {
		t.Fatalf("enhanced set = %+v", got)
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
