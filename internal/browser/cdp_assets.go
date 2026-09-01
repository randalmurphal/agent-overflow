package browser

import (
	"context"
	"encoding/base64"
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
	var raw pageAssets
	if err := chromedp.Run(ctx, chromedp.Evaluate(assetInventoryExpression(), &raw)); err != nil {
		return pageAssets{}, fmt.Errorf("browser: list page assets: %w", err)
	}
	return raw, nil
}

func (p *cdpPage) AssetFetcher(ctx context.Context) (assetFetcher, error) {
	frameTree, err := page.GetFrameTree().Do(targetCommandContext(ctx))
	if err != nil {
		return nil, err
	}
	frameID := frameTree.Frame.ID
	return func(url string) (assetStream, error) {
		result, loadErr := network.LoadNetworkResource(url, &network.LoadNetworkResourceOptions{DisableCache: false, IncludeCredentials: true}).WithFrameID(frameID).Do(targetCommandContext(ctx))
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
			Close: func() { _ = cdpio.Close(result.Stream).Do(targetCommandContext(ctx)) },
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
		var read cdpio.ReadReturns
		err := cdp.Execute(targetCommandContext(ctx), cdpio.CommandRead, cdpio.Read(handle).WithSize(1<<20), &read)
		if err != nil {
			return written, err
		}
		chunk := []byte(read.Data)
		if read.Base64encoded {
			chunk, err = base64.StdEncoding.DecodeString(read.Data)
			if err != nil {
				return written, err
			}
		}
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
