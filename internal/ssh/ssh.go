package ssh

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
	cryptossh "golang.org/x/crypto/ssh"
	"k8s.io/utils/exec"
)

// Config defines SSH configuration.
type Config struct {
	Logger  *zap.Logger
	KeyPath string

	PublicIP      string
	PublicDNSName string

	// UserName is the user name to use for log-in.
	// "ec2-user" for Amazon Linux 2
	// "ubuntu" for ubuntu
	UserName string

	Envs map[string]string
}

// SSH defines SSH operations.
type SSH interface {
	// Connect connects to a remote server creating a new client session.
	// "Close" must be called after use.
	Connect() error
	// Close closes the session and connection to a remote server.
	Close()
	// Run runs the command and returns the output.
	Run(cmd string, opts ...OpOption) (out []byte, err error)
	// Send sends a file to the remote host using SCP protocol.
	Send(localPath, remotePath string, opts ...OpOption) (out []byte, err error)
	// Download downloads a file from the remote host using SCP protocol.
	Download(remotePath, localPath string, opts ...OpOption) (out []byte, err error)
}

type ssh struct {
	cfg Config

	lg *zap.Logger

	key    []byte
	signer cryptossh.Signer

	ctx    context.Context
	cancel context.CancelFunc

	conn net.Conn
	cli  *cryptossh.Client

	retries map[string]int
}

// New returns a new SSH.
func New(cfg Config) (s SSH, err error) {
	sh := &ssh{
		cfg:     cfg,
		lg:      cfg.Logger,
		retries: make(map[string]int),
	}
	if sh.lg == nil {
		sh.lg = zap.NewNop()
	}
	return sh, nil
}

func (sh *ssh) Connect() (err error) {
	sh.ctx, sh.cancel = context.WithCancel(context.Background())
	sh.key, err = ioutil.ReadFile(sh.cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key %v", err)
	}
	sh.signer, err = cryptossh.ParsePrivateKey(sh.key)
	if err != nil {
		return fmt.Errorf("failed to parse private key %v", err)
	}

	sh.lg.Info("dialing",
		zap.String("public-ip", sh.cfg.PublicIP),
		zap.String("public-dns-name", sh.cfg.PublicDNSName),
	)
	for i := 0; i < 15; i++ {
		select {
		case <-sh.ctx.Done():
			return errors.New("stopped")
		default:
		}

		d := net.Dialer{}
		ctx, cancel := context.WithTimeout(sh.ctx, 15*time.Second)
		sh.conn, err = d.DialContext(ctx, "tcp", sh.cfg.PublicIP+":22")
		cancel()
		if err == nil {
			break
		}

		oerr, ok := err.(*net.OpError)
		if ok {
			// connect: connection refused
			if strings.Contains(oerr.Err.Error(), syscall.ECONNREFUSED.Error()) {
				sh.lg.Warn(
					"failed to dial (instance might not be ready yet)",
					zap.String("public-ip", sh.cfg.PublicIP),
					zap.String("public-dns-name", sh.cfg.PublicDNSName),
					zap.Error(err),
				)
			}
		} else {
			sh.lg.Warn(
				"failed to dial",
				zap.String("public-ip", sh.cfg.PublicIP),
				zap.String("public-dns-name", sh.cfg.PublicDNSName),
				zap.String("error-type", fmt.Sprintf("%v", reflect.TypeOf(err))),
				zap.Error(err),
			)
		}
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		return err
	}
	sh.lg.Info("dialed",
		zap.String("public-ip", sh.cfg.PublicIP),
		zap.String("public-dns-name", sh.cfg.PublicDNSName),
	)

	sshConfig := &cryptossh.ClientConfig{
		User: sh.cfg.UserName,
		Auth: []cryptossh.AuthMethod{
			cryptossh.PublicKeys(sh.signer),
		},
		HostKeyCallback: cryptossh.InsecureIgnoreHostKey(),
	}
	c, chans, reqs, err := cryptossh.NewClientConn(sh.conn, sh.cfg.PublicIP+":22", sshConfig)
	if err != nil {
		fi, _ := os.Stat(sh.cfg.KeyPath)
		sh.lg.Warn(
			"failed to connect",
			zap.String("public-ip", sh.cfg.PublicIP),
			zap.String("public-dns-name", sh.cfg.PublicDNSName),
			zap.String("file-mode", fi.Mode().String()),
			zap.String("error-type", fmt.Sprintf("%v", reflect.TypeOf(err))),
			zap.Error(err),
		)
		return err
	}

	sh.cli = cryptossh.NewClient(c, chans, reqs)
	sh.lg.Info("created client",
		zap.String("public-ip", sh.cfg.PublicIP),
		zap.String("public-dns-name", sh.cfg.PublicDNSName),
	)

	return err
}

func (sh *ssh) Close() {
	sh.cancel()
	cerr := sh.conn.Close()
	sh.lg.Info("closed connection",
		zap.String("public-ip", sh.cfg.PublicIP),
		zap.String("public-dns-name", sh.cfg.PublicDNSName),
		zap.Error(cerr),
	)
}

func (sh *ssh) Run(cmd string, opts ...OpOption) (out []byte, err error) {
	ret := Op{verbose: true, retries: 0, retryInterval: time.Duration(0), timeout: 0, envs: make(map[string]string)}
	ret.applyOpts(opts)

	key := fmt.Sprintf("%s%s", sh.cfg.PublicDNSName, cmd)
	if _, ok := sh.retries[key]; !ok {
		sh.retries[key] = ret.retries
	}

	now := time.Now().UTC()

	// session only accepts one call to Run, Start, Shell, Output, or CombinedOutput
	var ss *cryptossh.Session
	ss, err = sh.cli.NewSession()
	if err != nil {
		return nil, err
	}
	ss.Stderr = nil
	ss.Stdout = nil
	sh.lg.Info("created client session, running command", zap.String("cmd", cmd))

	if len(sh.cfg.Envs) > 0 {
		for k, v := range sh.cfg.Envs {
			if err = ss.Setenv(k, v); err != nil {
				return nil, err
			}
		}
	}
	if len(ret.envs) > 0 {
		for k, v := range ret.envs {
			if err = ss.Setenv(k, v); err != nil {
				return nil, err
			}
		}
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if ret.timeout == 0 {
		ctx, cancel = context.WithCancel(sh.ctx)
	} else {
		ctx, cancel = context.WithTimeout(sh.ctx, ret.timeout)
	}

	donec := make(chan error)
	go func() {
		out, err = ss.CombinedOutput(cmd)
		close(donec)
	}()
	select {
	case <-ctx.Done():
		ss.Close()
		cancel()
		<-donec
		out, err = nil, ctx.Err()
	case <-donec:
		ss.Close()
		cancel()
	}

	if ret.verbose {
		sh.lg.Info("ran command",
			zap.String("cmd", cmd),
			zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
		)
	}

	if err != nil {
		sh.lg.Warn("command failed", zap.Error(err))
		if sh.retries[key] != 0 {
			sh.lg.Warn("retrying", zap.Int("retries", sh.retries[key]))
			sh.Close()
			connErr := errors.New("")
			for connErr != nil {
				sh.retries[key]--
				connErr = sh.Connect()
			}
			time.Sleep(ret.retryInterval)
			out, err = sh.Run(cmd, opts...)
		}
	}
	if err == nil {
		delete(sh.retries, key)
	}
	return out, err
}

/*
chmod 400 /var/folders/y_/_dn293xd5kn7xlg6jvp7jpmxs99pm9/T/aws-k8s-tester-ec2.key301005900

ssh -o "StrictHostKeyChecking no" \
  -i /var/folders/y_/_dn293xd5kn7xlg6jvp7jpmxs99pm9/T/aws-k8s-tester-ec2.key669686897 \
  ec2-user@ec2-35-166-71-150.us-west-2.compute.amazonaws.com

rm -f ./text.txt
echo "Hello" > ./text.txt

scp -oStrictHostKeyChecking=no \
  -i /var/folders/y_/_dn293xd5kn7xlg6jvp7jpmxs99pm9/T/aws-k8s-tester-ec2.key301005900 \
  ./text.txt \
  ec2-user@ec2-35-166-71-150.us-west-2.compute.amazonaws.com:/home/ec2-user/test.txt


/usr/bin/scp -oStrictHostKeyChecking=no \
  -i /var/folders/y_/_dn293xd5kn7xlg6jvp7jpmxs99pm9/T/aws-k8s-tester-ec2.key301005900 \
  /var/folders/y_/_dn293xd5kn7xlg6jvp7jpmxs99pm9/T/testfile449686843 \
  ec2-user@34.220.64.30:22:/home/ec2-user/aws-k8s-tester.txt

scp -oStrictHostKeyChecking=no \
  -i /var/folders/y_/_dn293xd5kn7xlg6jvp7jpmxs99pm9/T/aws-k8s-tester-ec2.key301005900 \
  ec2-user@ec2-35-166-71-150.us-west-2.compute.amazonaws.com:/home/ec2-user/test.txt \
  ./test2.txt
*/

func (sh *ssh) Send(localPath, remotePath string, opts ...OpOption) (out []byte, err error) {
	ret := Op{verbose: true, retries: 0, retryInterval: time.Duration(0), timeout: 0, envs: make(map[string]string)}
	ret.applyOpts(opts)

	key := fmt.Sprintf("%s%s", sh.cfg.PublicDNSName, localPath)
	if _, ok := sh.retries[key]; !ok {
		sh.retries[key] = ret.retries
	}

	now := time.Now().UTC()

	var ctx context.Context
	var cancel context.CancelFunc
	if ret.timeout == 0 {
		ctx, cancel = context.WithCancel(sh.ctx)
	} else {
		ctx, cancel = context.WithTimeout(sh.ctx, ret.timeout)
	}

	scpCmd := exec.New()
	var scpPath string
	scpPath, err = scpCmd.LookPath("scp")
	if err != nil {
		cancel()
		return nil, err
	}
	if err = os.Chmod(sh.cfg.KeyPath, 0400); err != nil {
		cancel()
		return nil, err
	}

	cmd := scpCmd.CommandContext(ctx,
		scpPath,
		"-oStrictHostKeyChecking=no",
		"-i", sh.cfg.KeyPath,
		localPath,
		fmt.Sprintf("%s@%s:%s", sh.cfg.UserName, sh.cfg.PublicDNSName, remotePath),
	)
	out, err = cmd.CombinedOutput()
	cancel()

	if ret.verbose {
		fi, ferr := os.Stat(localPath)
		if ferr == nil {
			sh.lg.Info("sent",
				zap.String("size", humanize.Bytes(uint64(fi.Size()))),
				zap.String("output", string(out)),
				zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
			)
		} else {
			sh.lg.Info("failed to send",
				zap.String("output", string(out)),
				zap.Error(ferr),
				zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
			)
		}
	}

	if err != nil {
		sh.lg.Warn("command failed", zap.Error(err))

		if sh.retries[key] != 0 {
			sh.lg.Warn("retrying", zap.Int("retries", sh.retries[key]))
			sh.Close()
			connErr := errors.New("")
			for connErr != nil {
				sh.retries[key]--
				connErr = sh.Connect()
			}
			time.Sleep(ret.retryInterval)
			out, err = sh.Send(localPath, remotePath, opts...)
		}
	}
	if err == nil {
		delete(sh.retries, key)
	}
	return out, err
}

func (sh *ssh) Download(remotePath, localPath string, opts ...OpOption) (out []byte, err error) {
	ret := Op{verbose: true, retries: 0, retryInterval: time.Duration(0), timeout: 0, envs: make(map[string]string)}
	ret.applyOpts(opts)

	key := fmt.Sprintf("%s%s", sh.cfg.PublicDNSName, localPath)
	if _, ok := sh.retries[key]; !ok {
		sh.retries[key] = ret.retries
	}

	now := time.Now().UTC()

	var ctx context.Context
	var cancel context.CancelFunc
	if ret.timeout == 0 {
		ctx, cancel = context.WithCancel(sh.ctx)
	} else {
		ctx, cancel = context.WithTimeout(sh.ctx, ret.timeout)
	}

	scpCmd := exec.New()
	var scpPath string
	scpPath, err = scpCmd.LookPath("scp")
	if err != nil {
		cancel()
		return nil, err
	}
	if err = os.Chmod(sh.cfg.KeyPath, 0400); err != nil {
		cancel()
		return nil, err
	}
	cmd := scpCmd.CommandContext(ctx,
		scpPath,
		"-oStrictHostKeyChecking=no",
		"-i", sh.cfg.KeyPath,
		fmt.Sprintf("%s@%s:%s", sh.cfg.UserName, sh.cfg.PublicDNSName, remotePath),
		localPath,
	)
	out, err = cmd.CombinedOutput()
	cancel()

	if ret.verbose {
		fi, ferr := os.Stat(localPath)
		if ferr == nil {
			sh.lg.Info("downloaded",
				zap.String("size", humanize.Bytes(uint64(fi.Size()))),
				zap.String("output", string(out)),
				zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
			)
		} else {
			sh.lg.Info("failed to download",
				zap.String("output", string(out)),
				zap.Error(ferr),
				zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
			)
		}
	}

	if err != nil {
		sh.lg.Warn("command failed", zap.Error(err))

		if sh.retries[key] != 0 {
			sh.lg.Warn("retrying", zap.Int("retries", sh.retries[key]))
			sh.Close()
			connErr := errors.New("")
			for connErr != nil {
				sh.retries[key]--
				connErr = sh.Connect()
			}
			time.Sleep(ret.retryInterval)
			out, err = sh.Download(remotePath, localPath, opts...)
		}
	}
	if err == nil {
		delete(sh.retries, key)
	}
	return out, err
}
