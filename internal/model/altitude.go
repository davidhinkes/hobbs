package model

import "fmt"

// Datum is the reference for a vertical limit.
type Datum string

const (
	DatumMSL     Datum = "MSL"     // feet above mean sea level
	DatumAGL     Datum = "AGL"     // feet above ground level
	DatumFL      Datum = "FL"      // flight level (hundreds of feet, standard pressure)
	DatumSFC     Datum = "SFC"     // surface; Value is nil
	DatumUnl     Datum = "UNL"     // unlimited; Value is nil
	DatumUnknown Datum = "UNKNOWN" // not stated or not parseable; Value is nil
)

// Altitude is a vertical limit. Unknown is represented explicitly and must
// never be treated as zero: a nil Value with DatumUnknown means "not known".
type Altitude struct {
	// Value is feet for MSL/AGL and the flight level number (e.g. 370 for FL370)
	// for FL. Nil for SFC, UNL and UNKNOWN.
	Value *int  `json:"value,omitempty"`
	Datum Datum `json:"datum"`
}

func MSL(ft int) Altitude { return Altitude{Value: &ft, Datum: DatumMSL} }
func AGL(ft int) Altitude { return Altitude{Value: &ft, Datum: DatumAGL} }
func FL(n int) Altitude   { return Altitude{Value: &n, Datum: DatumFL} }
func SFC() Altitude       { return Altitude{Datum: DatumSFC} }
func Unlimited() Altitude { return Altitude{Datum: DatumUnl} }
func Unknown() Altitude   { return Altitude{Datum: DatumUnknown} }

// Known reports whether the altitude carries a usable value.
func (a Altitude) Known() bool { return a.Datum != DatumUnknown && a.Datum != "" }

// Validate checks the value/datum pairing.
func (a Altitude) Validate() error {
	switch a.Datum {
	case DatumMSL, DatumAGL, DatumFL:
		if a.Value == nil {
			return fmt.Errorf("altitude: datum %s requires a value", a.Datum)
		}
	case DatumSFC, DatumUnl, DatumUnknown:
		if a.Value != nil {
			return fmt.Errorf("altitude: datum %s must not have a value", a.Datum)
		}
	default:
		return fmt.Errorf("altitude: invalid datum %q", a.Datum)
	}
	return nil
}
