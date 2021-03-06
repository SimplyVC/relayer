package relayer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/cosmos/ibc-go/v3/modules/core/04-channel/types"
	"go.uber.org/zap"
)

// ActiveChannel represents an IBC channel and whether there is an active goroutine relaying packets against it.
type ActiveChannel struct {
	channel *types.IdentifiedChannel
	active  bool
}

// StartRelayer starts the main relaying loop and returns a channel that will contain any control-flow related errors.
func StartRelayer(ctx context.Context, src, dst *Chain, interchainquery Interchainquery, filter ChannelFilter, maxTxSize, maxMsgLength uint64) chan error {
	errorChan := make(chan error, 1)

	go relayerMainLoop(ctx, src, dst, interchainquery, filter, maxTxSize, maxMsgLength, errorChan)
	return errorChan
}

// relayerMainLoop is the main loop of the relayer.
func relayerMainLoop(ctx context.Context, src, dst *Chain, interchainquery Interchainquery, filter ChannelFilter, maxTxSize, maxMsgLength uint64, errCh chan<- error) {
	defer close(errCh)

	channels := make(chan *ActiveChannel, 10)
	var srcOpenChannels []*ActiveChannel

	for {
		// Query the list of channels on the src connection
		srcChannels, err := queryChannelsOnConnection(ctx, src)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				errCh <- err
			} else {
				errCh <- fmt.Errorf("error querying all channels on chain{%s}@connection{%s}: %v",
					src.ChainID(), src.ConnectionID(), err)
			}
			return
		}

		// Apply the channel filter rule (i.e. build allowlist, denylist or relay on all channels available)
		srcChannels = applyChannelFilterRule(filter, srcChannels)

		// Filter for open channels that are not already in our slice of open channels
		srcOpenChannels = filterOpenChannels(srcChannels, srcOpenChannels)

		// Spin up a goroutine to relay packets & acks for each channel that isn't already being relayed against
		for _, channel := range srcOpenChannels {
			if !channel.active {
				channel.active = true
				go relayUnrelayedPacketsAndAcks(ctx, src, dst, maxTxSize, maxMsgLength, channel, channels, interchainquery)
			}
		}

		for channel := range channels {
			channel.active = false
			break
		}

		// Make sure we are removing channels no longer in OPEN state from the slice of open channels
		for i, channel := range srcOpenChannels {
			if channel.channel.State != types.OPEN {
				srcOpenChannels[i] = srcOpenChannels[len(srcOpenChannels)-1]
				srcOpenChannels = srcOpenChannels[:len(srcOpenChannels)-1]
			}
		}
	}
}

// queryChannelsOnConnection queries all the channels associated with a connection on the src chain.
func queryChannelsOnConnection(ctx context.Context, src *Chain) ([]*types.IdentifiedChannel, error) {
	// Query the latest heights on src & dst
	srch, err := src.ChainProvider.QueryLatestHeight(ctx)
	if err != nil {
		return nil, err
	}

	// Query the list of channels for the connection on src
	var srcChannels []*types.IdentifiedChannel

	if err = retry.Do(func() error {
		srcChannels, err = src.ChainProvider.QueryConnectionChannels(ctx, srch, src.ConnectionID())
		return err
	}, retry.Context(ctx), RtyAtt, RtyDel, RtyErr, retry.OnRetry(func(n uint, err error) {
		src.log.Debug(
			"Failed to query connection channels",
			zap.String("conn_id", src.ConnectionID()),
			zap.Uint("attempt", n+1),
			zap.Uint("max_attempts", RtyAttNum),
			zap.Error(err),
		)
	})); err != nil {
		return nil, err
	}

	return srcChannels, nil
}

// filterOpenChannels takes a slice of channels and adds all the channels with OPEN state to a new slice of channels.
// NOTE: channels will not be added to the slice of open channels more than once.
func filterOpenChannels(channels []*types.IdentifiedChannel, openChannels []*ActiveChannel) []*ActiveChannel {

	// Filter for open channels
	for _, channel := range channels {
		if channel.State == types.OPEN {
			inSlice := false

			// Check if we have already added this channel to the slice of open channels
			for _, openChannel := range openChannels {
				if channel.ChannelId == openChannel.channel.ChannelId {
					inSlice = true
					break
				}
			}

			// We don't want to add channels to the slice of open channels that have already been added
			if !inSlice {
				openChannels = append(openChannels, &ActiveChannel{
					channel: channel,
					active:  false,
				})
			}
		}
	}

	return openChannels
}

// applyChannelFilterRule will use the given ChannelFilter's rule and channel list to build the appropriate list of
// channels to relay on.
func applyChannelFilterRule(filter ChannelFilter, channels []*types.IdentifiedChannel) []*types.IdentifiedChannel {
	switch filter.Rule {
	case allowList:
		var filteredChans []*types.IdentifiedChannel
		for _, c := range channels {
			if filter.InChannelList(c.ChannelId) {
				filteredChans = append(filteredChans, c)
			}
		}
		return filteredChans
	case denyList:
		var filteredChans []*types.IdentifiedChannel
		for _, c := range channels {
			if filter.InChannelList(c.ChannelId) {
				continue
			}
			filteredChans = append(filteredChans, c)
		}
		return filteredChans
	default:
		// handle all channels on connection
		return channels
	}
}

// relayUnrelayedPacketsAndAcks will relay all the pending packets and acknowledgements on both the src and dst chains.
func relayUnrelayedPacketsAndAcks(ctx context.Context, src, dst *Chain, maxTxSize, maxMsgLength uint64, srcChannel *ActiveChannel, channels chan<- *ActiveChannel, interchainquery Interchainquery) {
	// make goroutine signal its death, whether it's a panic or a return
	defer func() {
		channels <- srcChannel
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if interchainquery.Query {
				if err := relayInterchainqueries(ctx, src, dst, maxTxSize, maxMsgLength, uint64(interchainquery.Buffer)); err != nil {
					return
				}
			}
			if err := relayUnrelayedPackets(ctx, src, dst, maxTxSize, maxMsgLength, srcChannel.channel); err != nil {
				return
			}
			if err := relayUnrelayedAcks(ctx, src, dst, maxTxSize, maxMsgLength, srcChannel.channel); err != nil {
				return
			}

			time.Sleep(1000 * time.Millisecond)
		}
	}
}

func relayInterchainqueries(ctx context.Context, src, dst *Chain, maxTxSize, maxMsgLength, buffer uint64) error {
	childCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Fetch any unrelayed sequences depending on the channel order
	iqs, err := UnrelayedInterchainqueries(ctx, src, dst, buffer)
	if err != nil {
		src.log.Warn(
			"Error retrieving pending interchainquery instances",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.Error(err),
		)
		return err
	}

	if len(iqs) == 0 {
		src.log.Info(
			"No pending interchainquery instances in queue",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("dst_chain_id", dst.ChainID()),
		)
		return nil
	}

	src.log.Debug(
		"Unrelayed pending interchainquery instances",
		zap.String("src_chain_id", src.ChainID()),
		zap.String("dst_chain_id", dst.ChainID()),
	)

	go func() {
		err := RelayInterchainqueries(childCtx, src, dst, iqs, maxTxSize, maxMsgLength)
		if err != nil {
			src.log.Warn(
				"Submit interchainquery error",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("dst_chain_id", dst.ChainID()),
				zap.Error(err),
			)
		}
		cancel()
	}()

	<-childCtx.Done()
	if !errors.Is(childCtx.Err(), context.Canceled) {
		src.log.Warn(
			"Submit interchainquery error",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.Error(childCtx.Err()),
		)

		return childCtx.Err()
	}

	return nil
}

// relayUnrelayedPackets fetches unrelayed packet sequence numbers and attempts to relay the associated packets.
func relayUnrelayedPackets(ctx context.Context, src, dst *Chain, maxTxSize, maxMsgLength uint64, srcChannel *types.IdentifiedChannel) error {
	childCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Fetch any unrelayed sequences depending on the channel order
	sp, err := UnrelayedSequences(ctx, src, dst, srcChannel)
	if err != nil {
		src.log.Warn(
			"Error retrieving unrelayed sequences",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.Error(err),
		)
		return err
	}

	if len(sp.Src) > 0 {
		src.log.Debug(
			"Unrelayed source packets",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.Uint64s("seqs", sp.Src),
		)
	}

	if len(sp.Dst) > 0 {
		src.log.Debug(
			"Unrelayed destination packets",
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.Uint64s("seqs", sp.Dst),
		)
	}

	if !sp.Empty() {
		go func() {
			err := RelayPackets(childCtx, src, dst, sp, maxTxSize, maxMsgLength, srcChannel)
			if err != nil {
				src.log.Warn(
					"Relay packets error",
					zap.String("src_chain_id", src.ChainID()),
					zap.String("src_channel_id", srcChannel.ChannelId),
					zap.String("dst_chain_id", dst.ChainID()),
					zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
					zap.Error(err),
				)
			}
			cancel()
		}()

		// Wait until the context is cancelled (i.e. RelayPackets() finishes) or the context times out
		<-childCtx.Done()
		if !errors.Is(childCtx.Err(), context.Canceled) {
			src.log.Warn(
				"Relay packets error",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("src_channel_id", srcChannel.ChannelId),
				zap.String("dst_chain_id", dst.ChainID()),
				zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
				zap.Error(childCtx.Err()),
			)

			return childCtx.Err()
		}

	} else {
		src.log.Info(
			"No packets in queue",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.String("src_port_id", srcChannel.PortId),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.String("dst_port_id", srcChannel.Counterparty.PortId),
		)
	}

	return nil
}

// relayUnrelayedAcks fetches unrelayed acknowledgements and attempts to relay them.
func relayUnrelayedAcks(ctx context.Context, src, dst *Chain, maxTxSize, maxMsgLength uint64, srcChannel *types.IdentifiedChannel) error {
	childCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Fetch any unrelayed acks depending on the channel order
	ap, err := UnrelayedAcknowledgements(ctx, src, dst, srcChannel)
	if err != nil {
		src.log.Warn(
			"Error retrieving unrelayed acknowledgements",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.Error(err),
		)
		return err
	}

	if len(ap.Src) > 0 {
		src.log.Debug(
			"Unrelayed acknowledgements",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.Uint64s("acks", ap.Src),
		)
	}

	if len(ap.Dst) > 0 {
		src.log.Debug(
			"Unrelayed acknowledgements",
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.Uint64s("acks", ap.Dst),
		)
	}

	if !ap.Empty() {
		go func() {
			err := RelayAcknowledgements(childCtx, src, dst, ap, maxTxSize, maxMsgLength, srcChannel)
			if err != nil {
				src.log.Warn(
					"Relay acknowledgements error",
					zap.String("src_chain_id", src.ChainID()),
					zap.String("src_channel_id", srcChannel.ChannelId),
					zap.String("dst_chain_id", dst.ChainID()),
					zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
					zap.Error(err),
				)
			}
			cancel()
		}()

		// Wait until the context is cancelled (i.e. RelayAcknowledgements() finishes) or the context times out
		<-childCtx.Done()
		if !errors.Is(childCtx.Err(), context.Canceled) {
			src.log.Warn(
				"Relay acknowledgements error",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("src_channel_id", srcChannel.ChannelId),
				zap.String("dst_chain_id", dst.ChainID()),
				zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
				zap.Error(childCtx.Err()),
			)
			return childCtx.Err()
		}

	} else {
		src.log.Info(
			"No acknowledgements in queue",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.String("src_port_id", srcChannel.PortId),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.String("dst_port_id", srcChannel.Counterparty.PortId),
		)
	}

	return nil
}
