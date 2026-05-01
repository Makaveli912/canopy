package contract

import (
	"fmt"
	"reflect"
)

/* This file contains contract level PluginErrors */

const DefaultModule = "plugin"

// NewError() creates a plugin error
func NewError(code uint64, module, message string) *PluginError {
	return &PluginError{Code: code, Module: module, Msg: message}
}

// Error() implements the errors interface
func (p *PluginError) Error() string {
	return fmt.Sprintf("\nModule:  %s\nCode:    %d\nMessage: %s", p.Module, p.Code, p.Msg)
}

// ── Built-in error codes 1–14 (from Canopy template — do not modify) ─────────

func ErrPluginTimeout() *PluginError {
	return NewError(1, DefaultModule, "a plugin timeout occurred")
}

func ErrMarshal(err error) *PluginError {
	return NewError(2, DefaultModule, fmt.Sprintf("marshal() failed with err: %s", err.Error()))
}

func ErrUnmarshal(err error) *PluginError {
	return NewError(3, DefaultModule, fmt.Sprintf("unmarshal() failed with err: %s", err.Error()))
}

func ErrFailedPluginRead(err error) *PluginError {
	return NewError(4, DefaultModule, fmt.Sprintf("a plugin read failed with err: %s", err.Error()))
}

func ErrFailedPluginWrite(err error) *PluginError {
	return NewError(5, DefaultModule, fmt.Sprintf("a plugin write failed with err: %s", err.Error()))
}

func ErrInvalidPluginRespId() *PluginError {
	return NewError(6, DefaultModule, "plugin response id is invalid")
}

func ErrUnexpectedFSMToPlugin(t reflect.Type) *PluginError {
	return NewError(7, DefaultModule, fmt.Sprintf("unexpected FSM to plugin: %v", t))
}

func ErrInvalidFSMToPluginMMessage(t reflect.Type) *PluginError {
	return NewError(8, DefaultModule, fmt.Sprintf("invalid FSM to plugin: %v", t))
}

func ErrInsufficientFunds() *PluginError {
	return NewError(9, DefaultModule, "insufficient funds")
}

func ErrFromAny(err error) *PluginError {
	return NewError(10, DefaultModule, fmt.Sprintf("fromAny() failed with err: %s", err.Error()))
}

func ErrInvalidMessageCast() *PluginError {
	return NewError(11, DefaultModule, "the message cast failed")
}

func ErrInvalidAddress() *PluginError {
	return NewError(12, DefaultModule, "address is invalid")
}

func ErrInvalidAmount() *PluginError {
	return NewError(13, DefaultModule, "amount is invalid")
}

func ErrTxFeeBelowStateLimit() *PluginError {
	return NewError(14, DefaultModule, "tx.fee is below state limit")
}

// ── Praxis-specific error codes — start at 15 to avoid built-in conflicts ─────

// ErrWrongOutcome is returned when a claimer's prediction outcome does not
// match the market's winning outcome.
func ErrWrongOutcome() *PluginError {
	return NewError(15, DefaultModule, "prediction outcome does not match the winning outcome")
}

// ErrDuplicatePrediction is returned when a forecaster attempts to submit
// a second prediction on a market they have already predicted on.
func ErrDuplicatePrediction() *PluginError {
	return NewError(16, DefaultModule, "a prediction already exists for this forecaster and market")
}

// ErrEmptyQuestion is returned when a create_market message has no question text.
func ErrEmptyQuestion() *PluginError {
	return NewError(17, DefaultModule, "market question must not be empty")
}

// ErrInvalidOutcome is returned when an outcome value is not 1 (YES) or 2 (NO).
func ErrInvalidOutcome() *PluginError {
	return NewError(18, DefaultModule, "outcome must be 1 (YES) or 2 (NO)")
}

// ErrMarketNotFound is returned when the market ID does not exist in state.
func ErrMarketNotFound() *PluginError {
	return NewError(19, DefaultModule, "market not found")
}

// ErrMarketClosed is returned when a prediction or resolution is attempted
// on a market that is no longer open (already resolved or cancelled).
func ErrMarketClosed() *PluginError {
	return NewError(20, DefaultModule, "market is not open")
}

// ErrResolutionTooEarly is returned when a resolver attempts to resolve a
// market before its declared resolution height has been reached.
func ErrResolutionTooEarly() *PluginError {
	return NewError(21, DefaultModule, "resolution height has not been reached yet")
}

// ErrMarketNotResolved is returned when a claim is attempted on a market
// that has not yet been resolved.
func ErrMarketNotResolved() *PluginError {
	return NewError(22, DefaultModule, "market has not been resolved yet")
}

// ErrNoPredictionFound is returned when a claimer has no prediction recorded
// for the given market.
func ErrNoPredictionFound() *PluginError {
	return NewError(23, DefaultModule, "no prediction found for this claimer and market")
}

// ErrAlreadyClaimed is returned when a winner attempts to claim winnings
// on a prediction they have already claimed.
func ErrAlreadyClaimed() *PluginError {
	return NewError(24, DefaultModule, "winnings have already been claimed for this prediction")
}
