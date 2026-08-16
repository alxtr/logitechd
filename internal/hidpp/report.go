// Package hidpp contains hardware-independent HID++ wire and descriptor
// primitives. It does not open devices or implement receiver operations.
package hidpp

import (
	"errors"
	"fmt"
	"sort"
)

// ErrNotHIDPPReport classifies raw reports that do not belong to the HID++
// short/long wire formats. A shared HIDRAW stream may contain ordinary input
// reports alongside HID++ traffic; those reports must not fail transactions.
var ErrNotHIDPPReport = errors.New("hidpp: not a HID++ report")

const (
	// ShortReportID is the HID report ID used by a seven-byte HID++ message.
	ShortReportID byte = 0x10
	// LongReportID is the HID report ID used by a twenty-byte HID++ message.
	LongReportID byte = 0x11
)

const (
	shortReportLength = 7
	longReportLength  = 20
	shortParameterLen = shortReportLength - 4
	longParameterLen  = longReportLength - 4
)

// ReportType identifies the fixed-size HID++ wire format.
type ReportType uint8

const (
	ReportTypeUnknown ReportType = iota
	ReportTypeShort
	ReportTypeLong
)

func (t ReportType) String() string {
	switch t {
	case ReportTypeShort:
		return "short"
	case ReportTypeLong:
		return "long"
	default:
		return "unknown"
	}
}

// Report is the common header and parameter area of a HID++ short or long
// message. Parameters always has the wire format's complete fixed capacity;
// unused bytes are represented as zero padding.
//
// Function and SoftwareID are the high and low nibbles of the fourth wire
// byte. Feature-access messages use those nibbles as function and software
// identifiers; register-access messages may instead treat CommandByte as a
// complete register address.
type Report struct {
	Type        ReportType
	DeviceIndex byte
	SubID       byte
	Function    byte
	SoftwareID  byte
	Parameters  []byte
}

// FeatureIndex returns SubID under the HID++ 2.0 terminology.
func (r Report) FeatureIndex() byte {
	return r.SubID
}

// ReportID returns the wire report ID associated with the report type.
func (r Report) ReportID() byte {
	switch r.Type {
	case ReportTypeShort:
		return ShortReportID
	case ReportTypeLong:
		return LongReportID
	default:
		return 0
	}
}

// CommandByte returns the fourth byte of the wire report.
func (r Report) CommandByte() byte {
	return r.Function<<4 | r.SoftwareID
}

// Parse decodes one complete HID++ short or long report. The input must have
// the exact wire length; accepting truncated or silently overlong messages
// would make report boundaries ambiguous.
func Parse(data []byte) (Report, error) {
	if len(data) == 0 {
		return Report{}, fmt.Errorf("%w: empty report", ErrNotHIDPPReport)
	}

	reportType, expectedLength, parameterLength, err := formatForID(data[0])
	if err != nil {
		return Report{}, err
	}
	if len(data) != expectedLength {
		return Report{}, fmt.Errorf("hidpp: report ID 0x%02x requires %d bytes, got %d", data[0], expectedLength, len(data))
	}

	commandByte := data[3]
	parameters := make([]byte, parameterLength)
	copy(parameters, data[4:])
	return Report{
		Type:        reportType,
		DeviceIndex: data[1],
		SubID:       data[2],
		Function:    commandByte >> 4,
		SoftwareID:  commandByte & 0x0f,
		Parameters:  parameters,
	}, nil
}

// ParseReport is an explicit alias for Parse.
func ParseReport(data []byte) (Report, error) {
	return Parse(data)
}

// Build encodes a HID++ report, padding its parameter area with zeroes. At
// most three bytes may be supplied for a short report and at most sixteen for
// a long report.
func Build(report Report) ([]byte, error) {
	_, length, parameterLength, err := formatForType(report.Type)
	if err != nil {
		return nil, err
	}
	if report.Function > 0x0f {
		return nil, fmt.Errorf("hidpp: function 0x%02x does not fit in four bits", report.Function)
	}
	if report.SoftwareID > 0x0f {
		return nil, fmt.Errorf("hidpp: software ID 0x%02x does not fit in four bits", report.SoftwareID)
	}
	if len(report.Parameters) > parameterLength {
		return nil, fmt.Errorf("hidpp: %s report accepts at most %d parameter bytes, got %d", report.Type, parameterLength, len(report.Parameters))
	}

	data := make([]byte, length)
	data[0] = report.ReportID()
	data[1] = report.DeviceIndex
	data[2] = report.SubID
	data[3] = report.CommandByte()
	copy(data[4:], report.Parameters)
	return data, nil
}

// BuildReport is an explicit alias for Build.
func BuildReport(report Report) ([]byte, error) {
	return Build(report)
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (r Report) MarshalBinary() ([]byte, error) {
	return Build(r)
}

func formatForID(id byte) (ReportType, int, int, error) {
	switch id {
	case ShortReportID:
		return ReportTypeShort, shortReportLength, shortParameterLen, nil
	case LongReportID:
		return ReportTypeLong, longReportLength, longParameterLen, nil
	default:
		return ReportTypeUnknown, 0, 0, fmt.Errorf("%w: unsupported report ID 0x%02x", ErrNotHIDPPReport, id)
	}
}

func formatForType(reportType ReportType) (byte, int, int, error) {
	switch reportType {
	case ReportTypeShort:
		return ShortReportID, shortReportLength, shortParameterLen, nil
	case ReportTypeLong:
		return LongReportID, longReportLength, longParameterLen, nil
	default:
		return 0, 0, 0, fmt.Errorf("hidpp: unsupported report type %d", reportType)
	}
}

// ReportFormat describes a HID++ format found in a raw HID report
// descriptor. Length is the canonical on-wire length, while
// DescriptorLength is the length implied by the descriptor, including its
// report ID byte. Some devices describe one trailing padding byte.
type ReportFormat struct {
	Type             ReportType
	ReportID         byte
	Length           int
	ParameterLength  int
	DescriptorLength int
	PaddingBytes     int
}

// Descriptor is the HID++ format subset recognized in a HID report
// descriptor.
type Descriptor struct {
	Formats []ReportFormat
}

// Format returns the recognized format for reportID.
func (d Descriptor) Format(reportID byte) (ReportFormat, bool) {
	for _, format := range d.Formats {
		if format.ReportID == reportID {
			return format, true
		}
	}
	return ReportFormat{}, false
}

// RecognizeDescriptor identifies HID++ short and long reports in a raw HID
// report descriptor. It understands the HID short and long item encodings,
// report IDs, report sizes/counts, and the three data main items. Both common
// descriptor conventions are accepted: report counts that describe the
// payload only and counts that include one byte of report-ID padding.
func RecognizeDescriptor(data []byte) (Descriptor, error) {
	lengths, err := descriptorReportLengths(data)
	if err != nil {
		return Descriptor{}, err
	}

	formats := make([]ReportFormat, 0, 2)
	for id, payloadLength := range lengths {
		var reportType ReportType
		var wireLength, parameterLength int
		switch {
		case id == ShortReportID && (payloadLength == shortReportLength-1 || payloadLength == shortReportLength):
			reportType = ReportTypeShort
			wireLength = shortReportLength
			parameterLength = shortParameterLen
		case id == LongReportID && (payloadLength == longReportLength-1 || payloadLength == longReportLength):
			reportType = ReportTypeLong
			wireLength = longReportLength
			parameterLength = longParameterLen
		default:
			continue
		}

		descriptorLength := payloadLength + 1
		padding := descriptorLength - wireLength
		if padding < 0 {
			padding = 0
		}
		formats = append(formats, ReportFormat{
			Type:             reportType,
			ReportID:         id,
			Length:           wireLength,
			ParameterLength:  parameterLength,
			DescriptorLength: descriptorLength,
			PaddingBytes:     padding,
		})
	}

	sort.Slice(formats, func(i, j int) bool { return formats[i].ReportID < formats[j].ReportID })
	if len(formats) == 0 {
		return Descriptor{}, fmt.Errorf("hidpp: descriptor contains no recognized short or long report")
	}
	return Descriptor{Formats: formats}, nil
}

// RecognizeReportDescriptor is an explicit alias for RecognizeDescriptor.
func RecognizeReportDescriptor(data []byte) (Descriptor, error) {
	return RecognizeDescriptor(data)
}

type reportItemKind uint8

const (
	itemInput reportItemKind = iota + 1
	itemOutput
	itemFeature
)

// descriptorReportLengths returns the payload length for each report ID. HID
// permits a report to be split across multiple main items and permits input,
// output, and feature reports to have separate lengths; the largest complete
// direction is the useful value for format recognition.
func descriptorReportLengths(data []byte) (map[byte]int, error) {
	type globalState struct {
		reportSize  int
		reportCount int
		reportID    byte
	}

	state := globalState{}
	stack := make([]globalState, 0, 4)
	lengths := make(map[byte]map[reportItemKind]int)
	for offset := 0; offset < len(data); {
		prefix := data[offset]
		offset++

		var itemType, tag byte
		var payload []byte
		if prefix == 0xfe {
			if len(data)-offset < 2 {
				return nil, fmt.Errorf("hidpp: truncated long descriptor item at byte %d", offset-1)
			}
			size := int(data[offset])
			offset++
			offset++
			if size > len(data)-offset {
				return nil, fmt.Errorf("hidpp: truncated long descriptor payload at byte %d", offset-3)
			}
			offset += size
			// Long items are not needed for report accounting.
			continue
		}

		sizeCode := int(prefix & 0x03)
		size := sizeCode
		if sizeCode == 3 {
			size = 4
		}
		itemType = (prefix >> 2) & 0x03
		tag = (prefix >> 4) & 0x0f
		if size > len(data)-offset {
			return nil, fmt.Errorf("hidpp: truncated descriptor item at byte %d", offset-1)
		}
		payload = data[offset : offset+size]
		offset += size

		switch itemType {
		case 1: // Global
			switch tag {
			case 0x07: // Report Size
				value, ok := unsignedItem(payload)
				if !ok || value > uint64(^uint(0)>>1) {
					return nil, fmt.Errorf("hidpp: invalid report size at byte %d", offset-size)
				}
				state.reportSize = int(value)
			case 0x08: // Report ID
				if len(payload) != 1 || payload[0] == 0 {
					return nil, fmt.Errorf("hidpp: invalid report ID item at byte %d", offset-size)
				}
				state.reportID = payload[0]
			case 0x09: // Report Count
				value, ok := unsignedItem(payload)
				if !ok || value > uint64(^uint(0)>>1) {
					return nil, fmt.Errorf("hidpp: invalid report count at byte %d", offset-size)
				}
				state.reportCount = int(value)
			case 0x0a: // Push
				stack = append(stack, state)
			case 0x0b: // Pop
				if len(stack) == 0 {
					return nil, fmt.Errorf("hidpp: global pop without push at byte %d", offset-size)
				}
				state = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
		case 0: // Main
			var kind reportItemKind
			switch tag {
			case 0x08:
				kind = itemInput
			case 0x09:
				kind = itemOutput
			case 0x0b:
				kind = itemFeature
			default:
				continue
			}
			if state.reportSize == 0 || state.reportCount == 0 {
				return nil, fmt.Errorf("hidpp: report item without size/count at byte %d", offset-size)
			}
			if state.reportSize > (int(^uint(0)>>1))/state.reportCount {
				return nil, fmt.Errorf("hidpp: report size overflows at byte %d", offset-size)
			}
			bits := state.reportSize * state.reportCount
			payloadLength := bits / 8
			if bits%8 != 0 {
				payloadLength++
			}
			byKind := lengths[state.reportID]
			if byKind == nil {
				byKind = make(map[reportItemKind]int)
				lengths[state.reportID] = byKind
			}
			current := byKind[kind]
			if current > int(^uint(0)>>1)-payloadLength {
				return nil, fmt.Errorf("hidpp: report length overflows at byte %d", offset-size)
			}
			byKind[kind] = current + payloadLength
		}
	}

	result := make(map[byte]int, len(lengths))
	for reportID, byKind := range lengths {
		for _, payloadLength := range byKind {
			if payloadLength > result[reportID] {
				result[reportID] = payloadLength
			}
		}
	}
	return result, nil
}

func unsignedItem(data []byte) (uint64, bool) {
	if len(data) == 0 || len(data) > 8 {
		return 0, false
	}
	var value uint64
	for i, part := range data {
		value |= uint64(part) << (8 * i)
	}
	return value, true
}
