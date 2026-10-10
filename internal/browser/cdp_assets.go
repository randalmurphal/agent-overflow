package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	cdpio "github.com/chromedp/cdproto/io"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func (p *cdpPage) AssetInventory(ctx context.Context) (pageAssets, error) {
	raw, err := chromedp.Run(ctx, chromedp.Evaluate[pageAssets](assetInventoryExpression()))
	if err != nil {
		return pageAssets{}, fmt.Errorf("browser: list page assets: %w", err)
	}
	return raw, nil
}

func (p *cdpPage) AssetFetcher(ctx context.Context) (assetFetcher, error) {
	tree, err := chromedp.Call(ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return nil, err
	}
	if tree.FrameTree == nil || tree.FrameTree.Frame == nil {
		return nil, errors.New("browser: the page reported no main frame")
	}
	frameID := tree.FrameTree.Frame.ID
	return func(url string) (assetStream, error) {
		loaded, loadErr := chromedp.Call(ctx, network.LoadNetworkResource, network.LoadNetworkResourceParams{
			FrameID: frameID, URL: url, Options: &network.LoadNetworkResourceOptions{DisableCache: false, IncludeCredentials: true},
		})
		result := loaded.Resource
		if loadErr != nil || result == nil || !result.Success || result.Stream == "" {
			reason := "load failed"
			if loadErr != nil {
				reason = loadErr.Error()
			} else if result != nil && result.NetErrorName != "" {
				reason = result.NetErrorName
			}
			return assetStream{}, errors.New(reason)
		}
		stream := assetStream{
			Copy: func(out io.Writer, perFile, remaining int64) (int64, error) {
				return readCDPStream(ctx, result.Stream, out, perFile, remaining)
			},
			Close: func() { _, _ = chromedp.Call(ctx, cdpio.Close, cdpio.CloseParams{Handle: result.Stream}) },
		}
		for key, value := range result.Headers {
			if strings.EqualFold(key, "content-type") {
				stream.ContentType = fmt.Sprint(value)
				break
			}
		}
		return stream, nil
	}, nil
}

func readCDPStream(ctx context.Context, handle cdpio.StreamHandle, out io.Writer, perFile, remaining int64) (int64, error) {
	var written int64
	for {
		// io.ReadResult decodes base64 data itself.
		read, err := chromedp.Call(ctx, cdpio.Read, cdpio.ReadParams{Handle: handle, Size: 1 << 20})
		if err != nil {
			return written, err
		}
		chunk := read.Data
		if int64(len(chunk))+written > perFile || int64(len(chunk))+written > remaining {
			return written, fmt.Errorf("browser: asset exceeds bundle size limit")
		}
		n, err := out.Write(chunk)
		written += int64(n)
		if err != nil {
			return written, err
		}
		if read.EOF {
			return written, nil
		}
	}
}
