package cloudinv

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One part of a scope refusing must cost that part alone, and the caller has to
// be told which part, or a tool that deletes reports a clean account while the
// unread region still holds live resources.
func TestFanOutKeepsWhatItCouldReadAndNamesWhatItCouldNot(t *testing.T) {
	t.Parallel()

	read := func(ctx context.Context, part string) ([]string, error) {
		if part == "us-east-1" {
			return nil, errors.New("permission denied")
		}
		return []string{part + "/one"}, nil
	}

	found, incomplete, err := FanOut(context.Background(),
		[]string{"us-east-1", "us-west-2", "eu-west-1"}, func(error) Fault { return FaultDenied }, read)
	require.NoError(t, err)

	assert.Equal(t, []string{"us-west-2/one", "eu-west-1/one"}, found)
	assert.True(t, incomplete.Any())
	assert.False(t, incomplete.Total())
	assert.Equal(t, 3, incomplete.Asked)
	assert.Contains(t, incomplete.String(), "us-east-1 (permission denied)")
}

// Every part refusing is a different answer from an empty cloud, and only the
// count the list asked for tells them apart.
func TestFanOutReportsATotalFailureApartFromAPartialOne(t *testing.T) {
	t.Parallel()

	refuse := func(ctx context.Context, part string) ([]string, error) {
		return nil, errors.New("permission denied")
	}

	_, incomplete, err := FanOut(context.Background(),
		[]string{"us-east-1", "us-west-2"}, func(error) Fault { return FaultDenied }, refuse)
	require.NoError(t, err)

	assert.True(t, incomplete.Total())
}

// An empty cloud is not an incomplete read, which is the case a consumer must
// be able to report as "nothing here" without hedging.
func TestFanOutSaysNothingIsMissingWhenEveryPartAnswered(t *testing.T) {
	t.Parallel()

	empty := func(ctx context.Context, part string) ([]string, error) { return nil, nil }

	found, incomplete, err := FanOut(context.Background(),
		[]string{"us-east-1"}, func(error) Fault { return FaultUnknown }, empty)
	require.NoError(t, err)

	assert.Empty(t, found)
	assert.False(t, incomplete.Any())
	assert.False(t, incomplete.Total())
}

func TestFanOutStopsOnCancellation(t *testing.T) {
	t.Parallel()

	asked := 0
	count := func(ctx context.Context, part string) ([]string, error) {
		asked++
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := FanOut(ctx, []string{"a", "b"}, func(error) Fault { return FaultUnknown }, count)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, asked)
}

// A scope that asked for nothing in particular is not a scope that asked for
// nothing: Total reads false so a caller does not mistake it for a failure.
func TestIncompleteOnAnEmptyScopeIsNotATotalFailure(t *testing.T) {
	t.Parallel()

	_, incomplete, err := FanOut(context.Background(), nil, func(error) Fault { return FaultUnknown },
		func(ctx context.Context, part string) ([]string, error) { return nil, nil })
	require.NoError(t, err)

	assert.False(t, incomplete.Total())
	assert.Zero(t, incomplete.Asked)
}

// A consumer wraps this in its own error, and the provider's message and the
// chain errors.As walks have to survive that wrapping.
func TestIncompleteCarriesACauseAConsumerCanWrap(t *testing.T) {
	t.Parallel()

	refused := &probeError{"the API is switched off"}
	_, incomplete, err := FanOut(context.Background(), []string{"one"},
		func(error) Fault { return FaultAPIDisabled },
		func(ctx context.Context, part string) ([]string, error) { return nil, refused })
	require.NoError(t, err)

	wrapped := fmt.Errorf("could not list in any of the %d parts: %w", incomplete.Asked, incomplete.Cause())

	var found *probeError
	require.True(t, errors.As(wrapped, &found), "the cause did not survive wrapping")
	assert.Contains(t, wrapped.Error(), "the API is switched off")
}

// An empty read has no cause, so a consumer does not wrap nil.
func TestIncompleteWithNoGapsHasNoCause(t *testing.T) {
	t.Parallel()

	assert.NoError(t, Incomplete{}.Cause())
}

type probeError struct{ msg string }

func (p *probeError) Error() string { return p.msg }
