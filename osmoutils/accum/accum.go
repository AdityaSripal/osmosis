package accum

import (
	"errors"
	"fmt"

	"github.com/cosmos/cosmos-sdk/store"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/osmosis-labs/osmosis/osmoutils"
)

// We keep this object as a way to interface with the methods, even though
// only the Accumulator inside is stored in state
type AccumulatorObject struct {
	// Store where accumulator is stored
	store store.KVStore

	// Accumulator's name (pulled from AccumulatorContent)
	name string

	// Accumulator's current value (pulled from AccumulatorContent)
	value sdk.DecCoins
}

// Makes a new accumulator at store/accum/{accumName}
// Returns error if already exists / theres some overlapping keys
func MakeAccumulator(accumStore store.KVStore, accumName string) error {
	if accumStore.Has(formatAccumPrefixKey(accumName)) {
		return errors.New("Accumulator with given name already exists in store")
	}

	// New accumulator values start out at zero
	// TODO: consider whether this should be a parameter instead of always zero
	initAccumValue := sdk.NewDecCoins()

	newAccum := AccumulatorObject{accumStore, accumName, initAccumValue}

	// Stores accumulator in state
	setAccumulator(newAccum, initAccumValue)

	return nil
}

// Gets the current value of the accumulator corresponding to accumName in accumStore
func GetAccumulator(accumStore store.KVStore, accumName string) (AccumulatorObject, error) {
	accumContent := AccumulatorContent{}
	found, err := osmoutils.Get(accumStore, formatAccumPrefixKey(accumName), &accumContent)
	if err != nil {
		return AccumulatorObject{}, err
	}
	if !found {
		return AccumulatorObject{}, AccumDoesNotExistError{AccumName: accumName}
	}

	accum := AccumulatorObject{accumStore, accumName, accumContent.AccumValue}

	return accum, nil
}

func setAccumulator(accum AccumulatorObject, amt sdk.DecCoins) {
	newAccum := AccumulatorContent{amt}
	osmoutils.MustSet(accum.store, formatAccumPrefixKey(accum.name), &newAccum)
}

// UpdateAccumulator updates the accumulator's value by amt.
// It does so by incresing the value of the accumulator by
// the given amount. Persists to store. Mutates the receiver.
func (accum *AccumulatorObject) UpdateAccumulator(amt sdk.DecCoins) {
	accum.value = accum.value.Add(amt...)
	setAccumulator(*accum, accum.value)
}

// NewPosition creates a new position for the given name, with the given number of share units.
// The name can be an owner's address, or any other unique identifier for a position.
// It takes a snapshot of the current accumulator value, and sets the position's initial value to that.
// The position is initialized with empty unclaimed rewards
// If there is an existing position for the given address, it is overwritten.
func (accum AccumulatorObject) NewPosition(name string, numShareUnits sdk.Dec, options *Options) error {
	return accum.newPosition(name, numShareUnits, accum.value, options)
}

// NewPositionCustomAcc creates a new position for the given name, with the given number of share units.
// The name can be an owner's address, or any other unique identifier for a position.
// It sets the position's accumulator to the given value of customAccumulatorValue.
// All custom accumulator values must be non-negative.
// The position is initialized with empty unclaimed rewards
// If there is an existing position for the given address, it is overwritten.
func (accum AccumulatorObject) NewPositionCustomAcc(name string, numShareUnits sdk.Dec, customAccumulatorValue sdk.DecCoins, options *Options) error {
	if customAccumulatorValue.IsAnyNegative() {
		return NegativeCustomAccError{customAccumulatorValue}
	}
	return accum.newPosition(name, numShareUnits, customAccumulatorValue, options)
}

func (accum AccumulatorObject) newPosition(name string, numShareUnits sdk.Dec, positionAccumulatorInit sdk.DecCoins, options *Options) error {
	if err := options.validate(); err != nil {
		return err
	}
	createNewPosition(accum, positionAccumulatorInit, name, numShareUnits, sdk.NewDecCoins(), options)
	return nil
}

// AddToPosition adds newShares of shares to an existing position with the given name.
// This is functionally equivalent to claiming rewards, closing down the position, and
// opening a fresh one with the new number of shares. We can represent this behavior by
// claiming rewards and moving up the accumulator start value to its current value.
//
// An alternative approach is to simply generate an additional position every time an
// address adds to its position. We do not pursue this path because we want to ensure
// that withdrawal and claiming functions remain constant time and do not scale with the
// number of times a user has added to their position.
//
// Returns nil on success. Returns error when:
// - newShares are negative or zero.
// - there is no existing position at the given address
// - other internal or database error occurs.
func (accum AccumulatorObject) AddToPosition(name string, newShares sdk.Dec) error {
	return accum.AddToPositionCustomAcc(name, newShares, accum.value)
}

// AddToPositionCustomAcc adds newShares of shares to an existing position with the given name.
// This is functionally equivalent to claiming rewards, closing down the position, and
// opening a fresh one with the new number of shares. We can represent this behavior by
// claiming rewards. The accumulator of the new position is set to given customAccumulatorValue.
// All custom accumulator values must be non-negative. They must also be a superset of the
// old accumulator value associated with the position.
//
// An alternative approach is to simply generate an additional position every time an
// address adds to its position. We do not pursue this path because we want to ensure
// that withdrawal and claiming functions remain constant time and do not scale with the
// number of times a user has added to their position.
//
// Returns nil on success. Returns error when:
// - newShares are negative or zero.
// - there is no existing position at the given address
// - other internal or database error occurs.
func (accum AccumulatorObject) AddToPositionCustomAcc(name string, newShares sdk.Dec, customAccumulatorValue sdk.DecCoins) error {
	if !newShares.IsPositive() {
		return errors.New("Attempted to add a non-zero and non-negative number of shares to a position")
	}

	// Get addr's current position
	position, err := getPosition(accum, name)
	if err != nil {
		return err
	}

	if err := validateAccumulatorValue(customAccumulatorValue, position.InitAccumValue); err != nil {
		return err
	}

	// Save current number of shares and unclaimed rewards
	unclaimedRewards := getTotalRewards(accum, position)
	oldNumShares, err := accum.GetPositionSize(name)
	if err != nil {
		return err
	}

	// Update user's position with new number of shares while moving its unaccrued rewards
	// into UnclaimedRewards. Starting accumulator value is moved up to accum'scurrent value
	createNewPosition(accum, customAccumulatorValue, name, oldNumShares.Add(newShares), unclaimedRewards, position.Options)

	return nil
}

// RemovePosition removes the specified number of shares from a position. Specifically, it claims
// the unclaimed and newly accrued rewards and returns them alongside the redeemed shares. Then, it
// overwrites the position record with the updated number of shares. Since it accrues rewards, it
// also moves up the position's accumulator value to the current accum val.
func (accum AccumulatorObject) RemoveFromPosition(name string, numSharesToRemove sdk.Dec) error {
	return accum.RemoveFromPositionCustomAcc(name, numSharesToRemove, accum.value)
}

// RemovePositionCustomAcc removes the specified number of shares from a position. Specifically, it claims
// the unclaimed and newly accrued rewards and returns them alongside the redeemed shares. Then, it
// overwrites the position record with the updated number of shares. Since it accrues rewards, it
// also resets the position's accumulator value to the given customAccumulatorValue.
// All custom accumulator values must be non-negative. They must also be a superset of the
// old accumulator value associated with the position.
func (accum AccumulatorObject) RemoveFromPositionCustomAcc(name string, numSharesToRemove sdk.Dec, customAccumulatorValue sdk.DecCoins) error {
	// Cannot remove zero or negative shares
	if numSharesToRemove.LTE(sdk.ZeroDec()) {
		return fmt.Errorf("Attempted to remove no/negative shares (%s)", numSharesToRemove)
	}

	// Get addr's current position
	position, err := getPosition(accum, name)
	if err != nil {
		return err
	}

	if err := validateAccumulatorValue(customAccumulatorValue, position.InitAccumValue); err != nil {
		return err
	}

	// Ensure not removing more shares than exist
	if numSharesToRemove.GT(position.NumShares) {
		return fmt.Errorf("Attempted to remove more shares (%s) than exist in the position (%s)", numSharesToRemove, position.NumShares)
	}

	// Save current number of shares and unclaimed rewards
	unclaimedRewards := getTotalRewards(accum, position)
	oldNumShares, err := accum.GetPositionSize(name)
	if err != nil {
		return err
	}

	createNewPosition(accum, customAccumulatorValue, name, oldNumShares.Sub(numSharesToRemove), unclaimedRewards, position.Options)

	return nil
}

// UpdatePosition updates the position with the given name by adding or removing
// the given number of shares. If numShares is positive, it is equivalent to calling
// AddToPosition. If numShares is negative, it is equivalent to calling RemoveFromPosition.
// Also, it moves up the position's accumulator value to the current accum value.
// Fails with error if numShares is zero. Returns nil on success.
func (accum AccumulatorObject) UpdatePosition(name string, numShares sdk.Dec) error {
	return accum.UpdatePositionCustomAcc(name, numShares, accum.value)
}

// UpdatePositionCustomAcc updates the position with the given name by adding or removing
// the given number of shares. If numShares is positive, it is equivalent to calling
// AddToPositionCustomAcc. If numShares is negative, it is equivalent to calling RemoveFromPositionCustomAcc.
// Fails with error if numShares is zero. Returns nil on success.
// It also resets the position's accumulator value to the given customAccumulatorValue.
// All custom accumulator values must be non-negative. They must also be a superset of the
// old accumulator value associated with the position.
func (accum AccumulatorObject) UpdatePositionCustomAcc(name string, numShares sdk.Dec, customAccumulatorValue sdk.DecCoins) error {
	if numShares.Equal(sdk.ZeroDec()) {
		return ZeroSharesError
	}

	if numShares.IsNegative() {
		return accum.RemoveFromPositionCustomAcc(name, numShares.Neg(), customAccumulatorValue)
	}

	return accum.AddToPositionCustomAcc(name, numShares, customAccumulatorValue)
}

func (accum AccumulatorObject) deletePosition(name string) {
	accum.store.Delete(formatPositionPrefixKey(accum.name, name))
}

// GetPositionSize returns the number of shares the position corresponding to `addr`
// in accumulator `accum` has, or an error if no position exists.
func (accum AccumulatorObject) GetPositionSize(name string) (sdk.Dec, error) {
	position, err := getPosition(accum, name)
	if err != nil {
		return sdk.Dec{}, err
	}

	return position.NumShares, nil
}

// HasPosition returns true if a position with the given name exists,
// false otherwise. Returns error if internal database error occurs.
func (accum AccumulatorObject) HasPosition(name string) (bool, error) {
	_, err := getPosition(accum, name)

	if err != nil {
		isNoPositionError := errors.Is(err, NoPositionError{Name: name})
		if isNoPositionError {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// GetValue returns the current value of the accumulator.
func (accum AccumulatorObject) GetValue() sdk.DecCoins {
	return accum.value
}

// ClaimRewards claims the rewards for the given address, and returns the amount of rewards claimed.
// Upon claiming the rewards, the position at the current address is reset to have no
// unclaimed rewards. The position's accumulator is also set to the current accumulator value.
// Returns error if no position exists for the given address. Returns error if any
// database errors occur.
func (accum AccumulatorObject) ClaimRewards(positionName string) (sdk.Coins, error) {
	position, err := getPosition(accum, positionName)
	if err != nil {
		return sdk.Coins{}, NoPositionError{positionName}
	}

	totalRewards := getTotalRewards(accum, position)

	// Return the integer coins to the user
	// The remaining change is thrown away.
	// This is acceptable because we round in favour of the protocol.
	truncatedRewards, _ := totalRewards.TruncateDecimal()

	// remove the position from state entirely if numShares = zero
	if position.NumShares.Equal(sdk.ZeroDec()) {
		accum.deletePosition(positionName)
	} else { // else, create a completely new position, with no rewards
		createNewPosition(accum, accum.value, positionName, position.NumShares, sdk.NewDecCoins(), position.Options)
	}

	return truncatedRewards, nil
}