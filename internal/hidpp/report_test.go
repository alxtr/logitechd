package hidpp

import (
	"bytes"
	"testing"
)

func TestBuildAndParseShortReportWithPadding(t *testing.T) {
	want := Report{
		Type:        ReportTypeShort,
		DeviceIndex: 0x02,
		SubID:       0x1a,
		Function:    0x0b,
		SoftwareID:  0x04,
		Parameters:  []byte{0xde, 0xad},
	}

	data, err := Build(want)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte{ShortReportID, 0x02, 0x1a, 0xb4, 0xde, 0xad, 0x00}
	if !bytes.Equal(data, expected) {
		t.Fatalf("Build() = %x, want %x", data, expected)
	}

	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want.Parameters = []byte{0xde, 0xad, 0x00}
	if got.Type != want.Type || got.DeviceIndex != want.DeviceIndex || got.SubID != want.SubID || got.Function != want.Function || got.SoftwareID != want.SoftwareID || !bytes.Equal(got.Parameters, want.Parameters) {
		t.Fatalf("Parse() = %+v, want %+v", got, want)
	}
	if got.FeatureIndex() != got.SubID {
		t.Fatalf("FeatureIndex() = 0x%02x, want SubID 0x%02x", got.FeatureIndex(), got.SubID)
	}
	if got.CommandByte() != expected[3] {
		t.Fatalf("CommandByte() = 0x%02x, want 0x%02x", got.CommandByte(), expected[3])
	}
}

func TestBuildAndParseLongReport(t *testing.T) {
	parameters := make([]byte, longParameterLen)
	for i := range parameters {
		parameters[i] = byte(i + 1)
	}
	want := Report{
		Type:        ReportTypeLong,
		DeviceIndex: 0xff,
		SubID:       0x07,
		Function:    0x03,
		SoftwareID:  0x0f,
		Parameters:  parameters,
	}

	data, err := BuildReport(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != longReportLength || data[0] != LongReportID || data[3] != 0x3f {
		t.Fatalf("unexpected long report: len=%d data=%x", len(data), data)
	}
	got, err := ParseReport(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Parameters, parameters) {
		t.Fatalf("parameters = %x, want %x", got.Parameters, parameters)
	}
}

func TestReportRejectsMalformedIDsAndLengths(t *testing.T) {
	tests := [][]byte{
		{},
		{0x12, 0, 0, 0, 0, 0, 0},
		{ShortReportID},
		{ShortReportID, 0, 0, 0, 0, 0, 0, 0},
		{LongReportID, 0, 0, 0},
	}
	for _, data := range tests {
		if _, err := Parse(data); err == nil {
			t.Fatalf("Parse(%x) returned nil error", data)
		}
	}
}

func TestBuildRejectsInvalidFields(t *testing.T) {
	tests := []Report{
		{Type: ReportTypeUnknown},
		{Type: ReportTypeShort, Function: 0x10},
		{Type: ReportTypeShort, SoftwareID: 0x10},
		{Type: ReportTypeShort, Parameters: []byte{1, 2, 3, 4}},
	}
	for _, report := range tests {
		if _, err := Build(report); err == nil {
			t.Fatalf("Build(%+v) returned nil error", report)
		}
	}
}

func TestRecognizeDescriptorWithPayloadCounts(t *testing.T) {
	descriptor := []byte{
		0x05, 0x01, // Usage Page (generic desktop)
		0x09, 0x06, // Usage (keyboard)
		0xa1, 0x01, // Collection (application)
		0x85, 0x10, // Report ID
		0x75, 0x08, // Report Size 8
		0x95, 0x06, // Report Count 6, excluding report ID
		0x81, 0x02, // Input
		0x85, 0x11, // Report ID
		0x95, 0x13, // Report Count 19, excluding report ID
		0x91, 0x02, // Output
		0xc0, // End collection
	}

	got, err := RecognizeDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Formats) != 2 {
		t.Fatalf("recognized %d formats, want 2: %+v", len(got.Formats), got.Formats)
	}
	short, ok := got.Format(ShortReportID)
	if !ok || short.Type != ReportTypeShort || short.Length != shortReportLength || short.DescriptorLength != 7 || short.PaddingBytes != 0 {
		t.Fatalf("short format = %+v, ok=%v", short, ok)
	}
	long, ok := got.Format(LongReportID)
	if !ok || long.Type != ReportTypeLong || long.Length != longReportLength || long.DescriptorLength != 20 || long.PaddingBytes != 0 {
		t.Fatalf("long format = %+v, ok=%v", long, ok)
	}
}

func TestRecognizeDescriptorWithReportIDPaddingAndGlobalStack(t *testing.T) {
	descriptor := []byte{
		0xa1, 0x01,
		0x75, 0x08,
		0xa4, // Push global state
		0x85, 0x10,
		0x95, 0x07, // one descriptor padding byte
		0x81, 0x02,
		0xb4, // Pop global state
		0x85, 0x11,
		0x95, 0x14, // one descriptor padding byte
		0xb1, 0x02, // Feature
		0xc0,
	}

	got, err := RecognizeReportDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	short, _ := got.Format(ShortReportID)
	long, _ := got.Format(LongReportID)
	if short.DescriptorLength != 8 || short.PaddingBytes != 1 || long.DescriptorLength != 21 || long.PaddingBytes != 1 {
		t.Fatalf("unexpected padded formats: short=%+v long=%+v", short, long)
	}
}

func TestRecognizeDescriptorWithSplitReportItems(t *testing.T) {
	descriptor := []byte{
		0x85, 0x10,
		0x75, 0x08,
		0x95, 0x03,
		0x81, 0x02,
		0x95, 0x03,
		0x81, 0x02,
	}

	got, err := RecognizeDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	short, ok := got.Format(ShortReportID)
	if !ok || short.DescriptorLength != shortReportLength || short.PaddingBytes != 0 {
		t.Fatalf("split short format = %+v, ok=%v", short, ok)
	}
}

func TestRecognizeDescriptorRejectsMalformedOrUnrelatedDescriptors(t *testing.T) {
	for name, descriptor := range map[string][]byte{
		"truncated item":    {0x75},
		"invalid report id": {0x85, 0x00, 0x75, 0x08, 0x95, 0x06, 0x81, 0x02},
		"no HID++ format":   {0x75, 0x08, 0x95, 0x01, 0x81, 0x02},
		"pop without push":  {0xb4},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RecognizeDescriptor(descriptor); err == nil {
				t.Fatalf("RecognizeDescriptor(%x) returned nil error", descriptor)
			}
		})
	}
}
