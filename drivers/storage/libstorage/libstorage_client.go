package libstorage

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	log "github.com/Sirupsen/logrus"

	gofig "github.com/akutz/gofig/types"
	"github.com/akutz/goof"
	"github.com/akutz/gotil"

	"github.com/codedellemc/libstorage/api/context"
	"github.com/codedellemc/libstorage/api/types"
	"github.com/codedellemc/libstorage/api/utils"
)

type client struct {
	types.APIClient
	ctx             types.Context
	config          gofig.Config
	tlsConfig       *types.TLSConfig
	pathConfig      *types.PathConfig
	clientType      types.ClientType
	lsxCache        *lss
	serviceCache    *lss
	supportedCache  *lss
	instanceIDCache types.Store
	lsxMutexPath    string
}

var errExecutorNotSupported = errors.New("executor not supported")

func (c *client) isController() bool {
	return c.clientType == types.ControllerClient
}

func (c *client) dial(ctx types.Context) error {

	svcInfos, err := c.Services(ctx)
	if err != nil {
		return err
	}

	// controller clients do not have any additional dialer logic
	if c.isController() {
		return nil
	}

	store := utils.NewStore()
	c.ctx = c.ctx.WithValue(context.ServerKey, c.ServerName())

	if !c.config.GetBool(types.ConfigExecutorNoDownload) {

		ctx.Info("initializing executors cache")
		if _, err := c.Executors(ctx); err != nil {
			return err
		}

		if err := c.updateExecutor(ctx); err != nil {
			return err
		}
	}

	for service := range svcInfos {
		ctx := c.ctx.WithValue(context.ServiceKey, service)
		ctx.Info("initializing supported cache")
		lsxSO, err := c.Supported(ctx, store)
		if err != nil {
			return goof.WithError("error initializing supported cache", err)
		}

		if lsxSO == types.LSXSOpNone {
			ctx.Warn("executor not supported")
			continue
		}

		ctx.Info("initializing instance ID cache")
		if _, err := c.InstanceID(ctx, store); err != nil {
			if err == types.ErrNotImplemented {
				ctx.WithError(err).Warn("cannot get instance ID")
				continue
			}
			return goof.WithError("error initializing instance ID cache", err)
		}
	}

	return nil
}

func getHost(
	ctx types.Context,
	proto, lAddr string, tlsConfig *types.TLSConfig) string {

	if tlsConfig != nil && tlsConfig.ServerName != "" {
		ctx.WithField("getHost", tlsConfig.ServerName).Debug(
			`getHost tlsConfig != nil && tlsConfig.ServerName != ""`)
		return tlsConfig.ServerName
	} else if proto == "unix" {
		ctx.WithField("getHost", "libstorage-server").Debug(
			`getHost proto == "unix"`)
		return "libstorage-server"
	} else {
		ctx.WithField("getHost", lAddr).Debug(
			`getHost lAddr`)
		return lAddr
	}
}

func (c *client) getServiceInfo(service string) (*types.ServiceInfo, error) {

	if si := c.serviceCache.GetServiceInfo(service); si != nil {
		return si, nil
	}
	return nil, goof.WithField("name", service, "unknown service")
}

func (c *client) updateExecutor(ctx types.Context) error {

	if c.isController() {
		return utils.NewUnsupportedForClientTypeError(
			c.clientType, "updateExecutor")
	}

	ctx.Debug("updating executor")

	lsxi := c.lsxCache.GetExecutorInfo(path.Base(c.pathConfig.LSX))
	if lsxi == nil {
		return goof.WithField("lsx", c.pathConfig.LSX, "unknown executor")
	}

	ctx.Debug("waiting on executor lock")
	if err := c.lsxMutexWait(); err != nil {
		return err
	}
	defer func() {
		ctx.Debug("signalling executor lock")
		if err := c.lsxMutexSignal(); err != nil {
			panic(err)
		}
	}()

	if !gotil.FileExists(c.pathConfig.LSX) {
		ctx.Debug("executor does not exist, download executor")
		return c.downloadExecutor(ctx)
	}

	ctx.Debug("executor exists, getting local checksum")

	checksum, err := c.getExecutorChecksum(ctx)
	if err != nil {
		return err
	}

	if lsxi.MD5Checksum != checksum {
		ctx.WithFields(log.Fields{
			"remoteChecksum": lsxi.MD5Checksum,
			"localChecksum":  checksum,
		}).Debug("executor checksums do not match, download executor")
		return c.downloadExecutor(ctx)
	}

	return nil
}

func (c *client) getExecutorChecksum(ctx types.Context) (string, error) {

	if c.isController() {
		return "", utils.NewUnsupportedForClientTypeError(
			c.clientType, "getExecutorChecksum")
	}

	ctx.Debug("getting executor checksum")

	f, err := os.Open(c.pathConfig.LSX)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	buf := make([]byte, 1024)
	for {
		n, err := f.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if _, err := h.Write(buf[:n]); err != nil {
			return "", err
		}
	}

	sum := fmt.Sprintf("%x", h.Sum(nil))
	ctx.WithField("localChecksum", sum).Debug("got local executor checksum")
	return sum, nil
}

func (c *client) downloadExecutor(ctx types.Context) error {

	if c.isController() {
		return utils.NewUnsupportedForClientTypeError(
			c.clientType, "downloadExecutor")
	}

	ctx.Debug("downloading executor")

	f, err := os.OpenFile(
		c.pathConfig.LSX,
		os.O_CREATE|os.O_RDWR|os.O_TRUNC,
		0755)
	if err != nil {
		return err
	}

	defer f.Close()

	rdr, err := c.APIClient.ExecutorGet(ctx, path.Base(c.pathConfig.LSX))
	if err != nil {
		return err
	}

	n, err := io.Copy(f, rdr)
	if err != nil {
		return err
	}

	if err := f.Sync(); err != nil {
		return err
	}

	ctx.WithField("bytes", n).Debug("downloaded executor")
	return nil
}
