package charger

// LICENSE

// Copyright (c) 2022-2025 premultiply

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Supports all chargers based on Bender CC612/613 controller series
// * The 'Modbus TCP Server for energy management systems' must be enabled.
// * The setting 'Register Address Set' must NOT be set to 'Phoenix', 'TQ-DM100' or 'ISE/IGT Kassel'.
//   -> Use the third selection labeled 'Ebee', 'Bender', 'MENNEKES' etc.
// * Set 'Allow UID Disclose' to On

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/evcc-io/evcc/util/sponsor"
)

// BenderCC charger implementation
type BenderCC struct {
	conn    *modbus.Connection
	current uint16
	regCurr uint16
	legacy  bool
}

const (
	// all holding type registers
	bendRegChargePointState   = 122  // Vehicle (Control Pilot) state
	bendRegPhaseEnergy        = 200  // Phase energy from primary meter (Wh)
	bendRegCurrents           = 212  // Currents from primary meter (mA)
	bendRegTotalEnergy        = 218  // Total Energy from primary meter (Wh)
	bendRegActivePower        = 220  // Active Power from primary meter (W)
	bendRegVoltages           = 222  // Voltages of the ocpp meter (V)
	bendRegUserID             = 720  // User ID (OCPP IdTag) from the current session. Bytes 0 to 19.
	bendRegEVBatteryState     = 730  // EV Battery State (% 0-100)
	bendRegEVCCID             = 741  // ASCII representation of the Hex. Values corresponding to the EVCCID. Bytes 0 to 11.
	bendRegHemsCurrentLimit   = 1000 // HEMS Current Limit (A)
	bendRegHemsCurrentLimit10 = 1001 // HEMS Current Limit 1/10 (0.1 A)
	bendRegHemsPowerLimit     = 1002 // HEMS Power Limit (W)

	bendRegFirmware             = 100 // Application version number
	bendRegOcppCpStatus         = 104 // Charge Point status according to the OCPP spec. enumaration
	bendRegProtocolVersion      = 120 // Ebee Modbus TCP Server Protocol Version number
	bendRegChargePointModel     = 142 // ChargePoint Model. Bytes 0 to 19.
	bendRegSmartVehicleDetected = 740 // Returns 1 if an EV currently connected is a smart vehicle, or 0 if no EV connected or it is not a smart vehicle

	// unused
	// bendRegChargedEnergyLegacy    = 705 // Sum of charged energy for the current session (Wh)
	// bendRegChargingDurationLegacy = 709 // Duration since beginning of charge (Seconds)
	// bendRegChargedEnergy          = 716 // Sum of charged energy for the current session (Wh)
	// bendRegChargingDuration       = 718 // Duration since beginning of charge (Seconds)

	powerLimit1p uint16 = 3725 // 207V * 3p * 6A - 1W
	powerLimit3p uint16 = 0xffff
)

func init() {
	registry.AddCtx("bender", NewBenderCCFromConfig)
}

// NewBenderCCFromConfig creates a BenderCC charger from generic config
func NewBenderCCFromConfig(ctx context.Context, other map[string]interface{}) (api.Charger, error) {
	cc := modbus.TcpSettings{
		ID: 255,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewBenderCC(ctx, cc.URI, cc.ID)
}

// NewBenderCC creates BenderCC charger
//
//go:generate go tool decorate -f decorateBenderCC -b *BenderCC -r api.Charger -t "api.Meter,CurrentPower,func() (float64, error)" -t "api.PhaseCurrents,Currents,func() (float64, float64, float64, error)" -t "api.PhaseVoltages,Voltages,func() (float64, float64, float64, error)" -t "api.MeterEnergy,TotalEnergy,func() (float64, error)" -t "api.Battery,Soc,func() (float64, error)" -t "api.Identifier,Identify,func() (string, error)" -t "api.ChargerEx,MaxCurrentMillis,func(float64) error" -t "api.PhaseSwitcher,Phases1p3p,func(int) error" -t "api.PhaseGetter,GetPhases,func() (int, error)"
func NewBenderCC(ctx context.Context, uri string, id uint8) (api.Charger, error) {
	conn, err := modbus.NewConnection(ctx, uri, "", "", 0, modbus.Tcp, id)
	if err != nil {
		return nil, err
	}

	if !sponsor.IsAuthorized() {
		return nil, api.ErrSponsorRequired
	}

	log := util.NewLogger("bender")
	conn.Logger(log.TRACE)

	wb := &BenderCC{
		conn:    conn,
		current: 6, // assume min current
		regCurr: bendRegHemsCurrentLimit,
	}

	// check legacy register set
	if _, err := wb.conn.ReadHoldingRegisters(bendRegChargePointModel, 10); err != nil {
		wb.legacy = true
	}

	var (
		currentPower     func() (float64, error)
		currents         func() (float64, float64, float64, error)
		voltages         func() (float64, float64, float64, error)
		totalEnergy      func() (float64, error)
		soc              func() (float64, error)
		identify         func() (string, error)
		maxCurrentMillis func(float64) error
		phases1p3p       func(int) error
		getPhases        func() (int, error)
	)

	// check presence of metering
	reg := uint16(bendRegActivePower)
	if wb.legacy {
		reg = bendRegPhaseEnergy
	}

	if b, err := wb.conn.ReadHoldingRegisters(reg, 2); err == nil && binary.BigEndian.Uint32(b) != math.MaxUint32 {
		currentPower = wb.currentPower
		currents = wb.currents
		totalEnergy = wb.totalEnergy

		// check presence of "ocpp meter"
		if b, err := wb.conn.ReadHoldingRegisters(bendRegVoltages, 2); err == nil && binary.BigEndian.Uint32(b) > 0 {
			voltages = wb.voltages
		}

		if !wb.legacy {
			if _, err := wb.conn.ReadHoldingRegisters(bendRegEVBatteryState, 1); err == nil {
				soc = wb.soc
			}
		}
	}

	// check feature mA
	if _, err := wb.conn.ReadHoldingRegisters(bendRegHemsCurrentLimit10, 1); err == nil {
		maxCurrentMillis = wb.maxCurrentMillis
		wb.regCurr = bendRegHemsCurrentLimit10
	}

	// check feature power control/1p3p
	if _, err := wb.conn.ReadHoldingRegisters(bendRegHemsPowerLimit, 1); err == nil {
		phases1p3p = wb.phases1p3p
		getPhases = wb.getPhases
	}

	// check feature rfid
	if _, err := wb.identify(); err == nil {
		identify = wb.identify
	}

	return decorateBenderCC(wb, currentPower, currents, voltages, totalEnergy, soc, identify, maxCurrentMillis, phases1p3p, getPhases), nil
}

// Status implements the api.Charger interface
func (wb *BenderCC) Status() (api.ChargeStatus, error) {
	b, err := wb.conn.ReadHoldingRegisters(bendRegChargePointState, 1)
	if err != nil {
		return api.StatusNone, err
	}

	switch s := binary.BigEndian.Uint16(b); s {
	case 1:
		return api.StatusA, nil
	case 2:
		return api.StatusB, nil
	case 3, 4:
		return api.StatusC, nil
	default:
		return api.StatusNone, fmt.Errorf("invalid status: %d", s)
	}
}

// Enabled implements the api.Charger interface
func (wb *BenderCC) Enabled() (bool, error) {
	b, err := wb.conn.ReadHoldingRegisters(wb.regCurr, 1)
	if err != nil {
		return false, err
	}

	return binary.BigEndian.Uint16(b) != 0, nil
}

// Enable implements the api.Charger interface
func (wb *BenderCC) Enable(enable bool) error {
	b := make([]byte, 2)
	if enable {
		binary.BigEndian.PutUint16(b, wb.current)
	}

	_, err := wb.conn.WriteMultipleRegisters(wb.regCurr, 1, b)

	return err
}

// MaxCurrent implements the api.Charger interface
func (wb *BenderCC) MaxCurrent(current int64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %d", current)
	}

	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, uint16(current))

	_, err := wb.conn.WriteMultipleRegisters(bendRegHemsCurrentLimit, 1, b)
	if err == nil {
		wb.current = uint16(current)
	}

	return err
}

// maxCurrentMillis implements the api.ChargerEx interface (Wallbe Firmware only)
func (wb *BenderCC) maxCurrentMillis(current float64) error {
	if current < 6 {
		return fmt.Errorf("invalid current %.5g", current)
	}

	curr := uint16(current * 10) // 0.1A Steps

	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, curr)

	_, err := wb.conn.WriteMultipleRegisters(bendRegHemsCurrentLimit10, 1, b)
	if err == nil {
		wb.current = curr
	}

	return err
}

// removed: https://github.com/evcc-io/evcc/issues/13555
// var _ api.ChargeTimer = (*BenderCC)(nil)

// CurrentPower implements the api.Meter interface
func (wb *BenderCC) currentPower() (float64, error) {
	if wb.legacy {
		l1, l2, l3, err := wb.currents()
		return 230 * (l1 + l2 + l3), err
	}

	b, err := wb.conn.ReadHoldingRegisters(bendRegActivePower, 2)
	if err != nil {
		return 0, err
	}

	return float64(binary.BigEndian.Uint32(b)), nil
}

// removed: https://github.com/evcc-io/evcc/issues/13726
// var _ api.ChargeRater = (*BenderCC)(nil)

// TotalEnergy implements the api.MeterEnergy interface
func (wb *BenderCC) totalEnergy() (float64, error) {
	if wb.legacy {
		b, err := wb.conn.ReadHoldingRegisters(bendRegPhaseEnergy, 6)
		if err != nil {
			return 0, err
		}

		var total float64
		for l := range 3 {
			total += float64(binary.BigEndian.Uint32(b[4*l:4*(l+1)])) / 1e3
		}

		return total, nil
	}

	b, err := wb.conn.ReadHoldingRegisters(bendRegTotalEnergy, 2)
	if err != nil {
		return 0, err
	}

	return float64(binary.BigEndian.Uint32(b)) / 1e3, nil
}

// getPhaseValues returns 3 sequential register values
func (wb *BenderCC) getPhaseValues(reg uint16, divider float64) (float64, float64, float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(reg, 6)
	if err != nil {
		return 0, 0, 0, err
	}

	var res [3]float64
	for i := range res {
		u32 := binary.BigEndian.Uint32(b[4*i:])
		if u32 == math.MaxUint32 {
			u32 = 0
		}
		res[i] = float64(u32) / divider
	}

	return res[0], res[1], res[2], nil
}

// currents implements the api.PhaseCurrents interface
func (wb *BenderCC) currents() (float64, float64, float64, error) {
	return wb.getPhaseValues(bendRegCurrents, 1e3)
}

// voltages implements the api.PhaseVoltages interface
func (wb *BenderCC) voltages() (float64, float64, float64, error) {
	return wb.getPhaseValues(bendRegVoltages, 1)
}

// phases1p3p implements the api.PhaseSwitcher interface
func (wb *BenderCC) phases1p3p(phases int) error {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, powerLimit3p)

	if phases == 1 {
		binary.BigEndian.PutUint16(b, powerLimit1p)
	}

	_, err := wb.conn.WriteMultipleRegisters(bendRegHemsPowerLimit, 1, b)

	return err
}

// getPhases implements the api.PhaseGetter interface
func (wb *BenderCC) getPhases() (int, error) {
	b, err := wb.conn.ReadHoldingRegisters(bendRegHemsPowerLimit, 1)
	if err != nil {
		return 0, err
	}

	if binary.BigEndian.Uint16(b) <= powerLimit1p {
		return 1, nil
	}

	return 3, nil
}

// identify implements the api.Identifier interface
func (wb *BenderCC) identify() (string, error) {
	if !wb.legacy {
		b, err := wb.conn.ReadHoldingRegisters(bendRegSmartVehicleDetected, 1)
		if err == nil && binary.BigEndian.Uint16(b) != 0 {
			b, err = wb.conn.ReadHoldingRegisters(bendRegEVCCID, 6)
		}

		if id := bytesAsString(b); id != "" || err != nil {
			return id, err
		}
	}

	b, err := wb.conn.ReadHoldingRegisters(bendRegUserID, 10)
	if err != nil {
		return "", err
	}

	return bytesAsString(b), nil
}

// soc implements the api.Battery interface
func (wb *BenderCC) soc() (float64, error) {
	b, err := wb.conn.ReadHoldingRegisters(bendRegSmartVehicleDetected, 1)
	if err != nil {
		return 0, err
	}

	if binary.BigEndian.Uint16(b) == 1 {
		b, err = wb.conn.ReadHoldingRegisters(bendRegEVBatteryState, 1)
		if err != nil {
			return 0, err
		}
		if soc := binary.BigEndian.Uint16(b); soc <= 100 {
			return float64(soc), nil
		}
	}

	return 0, api.ErrNotAvailable
}

var _ api.Diagnosis = (*BenderCC)(nil)

// Diagnose implements the api.Diagnosis interface
func (wb *BenderCC) Diagnose() {
	fmt.Printf("\tLegacy:\t\t%t\n", wb.legacy)
	if !wb.legacy {
		if b, err := wb.conn.ReadHoldingRegisters(bendRegChargePointModel, 10); err == nil {
			fmt.Printf("\tModel:\t%s\n", b)
		}
	}
	if b, err := wb.conn.ReadHoldingRegisters(bendRegFirmware, 2); err == nil {
		fmt.Printf("\tFirmware:\t%s\n", b)
	}
	if b, err := wb.conn.ReadHoldingRegisters(bendRegProtocolVersion, 2); err == nil {
		fmt.Printf("\tProtocol:\t%s\n", b)
	}
	if b, err := wb.conn.ReadHoldingRegisters(bendRegOcppCpStatus, 1); err == nil {
		fmt.Printf("\tOCPP Status:\t%d\n", binary.BigEndian.Uint16(b))
	}
	if !wb.legacy {
		if b, err := wb.conn.ReadHoldingRegisters(bendRegSmartVehicleDetected, 1); err == nil {
			fmt.Printf("\tSmart Vehicle:\t%t\n", binary.BigEndian.Uint16(b) != 0)
		}
	}
	if b, err := wb.conn.ReadHoldingRegisters(bendRegEVCCID, 6); err == nil {
		fmt.Printf("\tEVCCID:\t%s\n", b)
	}
	if b, err := wb.conn.ReadHoldingRegisters(bendRegUserID, 10); err == nil {
		fmt.Printf("\tUserID:\t%s\n", b)
	}
	if b, err := wb.conn.ReadHoldingRegisters(wb.regCurr, 1); err == nil {
		fmt.Printf("\tCurrent Limit:\t%d\n", binary.BigEndian.Uint16(b))
	}
}
