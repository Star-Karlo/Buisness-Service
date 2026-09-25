package services

import "os"

// WebFieldEnabled turns the receiving PIC's step on and off.
//
// TEMPORARY (Sept 2026). Nathanael is not ready to put an external warehouse
// PIC in the middle of a delivery: if the PIC does not answer, the driver
// cannot upload the unloading POD and the truck is held at the gate by a
// person who never agreed to be part of the flow. So the step is off, and the
// driver goes straight from the OTP to the POD.
//
// Off changes exactly two things, both here rather than scattered through the
// callers, so turning it back on is one variable:
//
//   - the unloading POD no longer waits for the PIC's cargo check;
//   - the PIC is not sent the Web-Field link, because nobody should be asked
//     to act on a page the flow no longer needs.
//
// Everything else stands: the code still goes to the driver, the driver still
// enters it to start unloading, and /webfield still works for anyone with a
// link — it is hidden, not removed. Set WEBFIELD_ENABLED=true to restore it.
var WebFieldEnabled = os.Getenv("WEBFIELD_ENABLED") == "true"
