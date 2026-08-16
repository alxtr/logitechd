package hidpp

import (
	"context"
	"fmt"
)

// HID++ 1.0 register sub-IDs. The command/address byte is the complete fourth
// wire byte for these operations, rather than a feature function/software ID
// pair.
const (
	RegisterSetSubID     byte = 0x80
	RegisterGetSubID     byte = 0x81
	RegisterLongSetSubID byte = 0x82
	RegisterLongGetSubID byte = 0x83
	RegisterErrorSubID   byte = 0x8f
)

const (
	shortRegisterParameterSize = shortParameterLen
	longRegisterParameterSize  = longParameterLen
)

// GetRegister reads one HID++ 1.0 register. size is the meaningful number of
// returned register bytes and selects the report format: one to three bytes
// use a short report, and four to sixteen bytes use a long report. The HID++
// long-register address range also selects a long report.
func (s *Session) GetRegister(ctx context.Context, deviceIndex byte, address uint16, size int) ([]byte, error) {
	return s.GetRegisterWithParameters(ctx, deviceIndex, address, size)
}

// GetRegisterWithParameters reads a register whose long-register request
// carries a subregister selector. The ordinary GetRegister API remains
// unchanged for existing callers.
func (s *Session) GetRegisterWithParameters(ctx context.Context, deviceIndex byte, address uint16, size int, requestParameters ...byte) ([]byte, error) {
	reportType, err := registerReportType(address, size)
	if err != nil {
		return nil, err
	}
	if len(requestParameters) > reportParameterCapacity(reportType) {
		return nil, unsupportedRegisterPayload(len(requestParameters))
	}
	command, subID, err := registerRequestFields(address, false)
	if err != nil {
		return nil, err
	}
	response, err := s.Exchange(ctx, Request{
		Report: Report{
			Type:        reportType,
			DeviceIndex: deviceIndex,
			SubID:       subID,
			Function:    command >> 4,
			SoftwareID:  command & 0x0f,
			Parameters:  append([]byte(nil), requestParameters...),
		},
		ResponseSubID: subID,
	})
	if err != nil {
		return nil, err
	}
	if len(response.Parameters) < size {
		return nil, malformedResponse(nil, fmt.Errorf("get-register response has %d parameters, need %d", len(response.Parameters), size))
	}
	return append([]byte(nil), response.Parameters[:size]...), nil
}

func reportParameterCapacity(reportType ReportType) int {
	if reportType == ReportTypeShort {
		return shortParameterLen
	}
	return longParameterLen
}

// SetRegister writes a HID++ 1.0 register and waits for the normal register
// acknowledgement. The payload length selects short or long format and must
// be between one and sixteen bytes.
func (s *Session) SetRegister(ctx context.Context, deviceIndex byte, address uint16, value []byte) error {
	return s.setRegister(ctx, deviceIndex, address, value, true)
}

// SetRegisterNoResponse writes a HID++ 1.0 register using the normal set
// sub-ID, but deliberately does not wait for an acknowledgement. HID++ 1.0
// has no separate no-response register sub-ID.
func (s *Session) SetRegisterNoResponse(ctx context.Context, deviceIndex byte, address uint16, value []byte) error {
	return s.setRegister(ctx, deviceIndex, address, value, false)
}

func (s *Session) setRegister(ctx context.Context, deviceIndex byte, address uint16, value []byte, wait bool) error {
	reportType, err := registerReportType(address, len(value))
	if err != nil {
		return err
	}
	command, subID, err := registerRequestFields(address, true)
	if err != nil {
		return err
	}
	request := Report{
		Type:        reportType,
		DeviceIndex: deviceIndex,
		SubID:       subID,
		Function:    command >> 4,
		SoftwareID:  command & 0x0f,
		Parameters:  append([]byte(nil), value...),
	}
	if wait {
		_, err = s.Exchange(ctx, Request{Report: request, ResponseSubID: subID})
		return err
	}
	return s.Send(ctx, request)
}

func registerReportType(address uint16, parameterSize int) (ReportType, error) {
	if _, _, err := registerRequestFields(address, false); err != nil {
		return ReportTypeUnknown, err
	}
	switch {
	case parameterSize >= 1 && parameterSize <= shortRegisterParameterSize:
		if address >= 0x200 {
			return ReportTypeLong, nil
		}
		return ReportTypeShort, nil
	case parameterSize >= shortRegisterParameterSize+1 && parameterSize <= longRegisterParameterSize:
		return ReportTypeLong, nil
	default:
		return ReportTypeUnknown, unsupportedRegisterPayload(parameterSize)
	}
}

func registerRequestFields(address uint16, write bool) (byte, byte, error) {
	switch {
	case address <= 0xff:
		if write {
			return byte(address), RegisterSetSubID, nil
		}
		return byte(address), RegisterGetSubID, nil
	case address >= 0x200 && address <= 0x2ff:
		if write {
			return byte(address), RegisterLongSetSubID, nil
		}
		return byte(address), RegisterLongGetSubID, nil
	default:
		return 0, 0, &UnsupportedError{
			Operation: "HID++ 1.0 register address",
			Detail:    fmt.Sprintf("0x%04x is outside the short and long register ranges", address),
		}
	}
}

func unsupportedRegisterPayload(parameterSize int) error {
	return &UnsupportedError{
		Operation: "HID++ 1.0 register payload",
		Detail:    fmt.Sprintf("%d bytes exceeds the long report capacity of %d or is empty", parameterSize, longRegisterParameterSize),
	}
}
