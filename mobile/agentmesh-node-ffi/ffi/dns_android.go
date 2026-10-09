//go:build android && cgo

package ffi

/*
#cgo LDFLAGS: -landroid
#include <android/multinetwork.h>
#include <errno.h>
#include <stdlib.h>

#pragma weak android_res_nquery
#pragma weak android_res_nresult
#pragma weak android_res_cancel

static int query_txt(const char *name) {
	if (!android_res_nquery || !android_res_nresult || !android_res_cancel) {
		return -ENOSYS;
	}
	return android_res_nquery(NETWORK_UNSPECIFIED, name, 1, 16, 0);
}
*/
import "C"

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
	"unsafe"

	madns "github.com/multiformats/go-multiaddr-dns"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

type androidDNSResolver struct {
	*net.Resolver
}

func (resolver androidDNSResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.ContainsRune(name, '\x00') {
		return nil, fmt.Errorf("invalid DNS name")
	}
	queryName := C.CString(name)
	defer C.free(unsafe.Pointer(queryName))
	queryFD := C.query_txt(queryName)
	if queryFD < 0 {
		if unix.Errno(-queryFD) == unix.ENOSYS {
			return nil, fmt.Errorf("TXT DNS resolution requires Android 10 or later")
		}
		return nil, fmt.Errorf("Android DNS query for %q: %w", name, unix.Errno(-queryFD))
	}
	if err := waitAndroidDNS(ctx, int(queryFD)); err != nil {
		C.android_res_cancel(queryFD)
		return nil, err
	}
	answer := make([]byte, 65535)
	var rcode C.int
	length := C.android_res_nresult(queryFD, &rcode, (*C.uint8_t)(unsafe.Pointer(&answer[0])), C.size_t(len(answer)))
	if length < 0 {
		return nil, fmt.Errorf("Android DNS response for %q: %w", name, unix.Errno(-length))
	}
	if rcode != 0 {
		return nil, &net.DNSError{
			Err:        fmt.Sprintf("DNS response code %d", rcode),
			Name:       name,
			IsNotFound: dnsmessage.RCode(rcode) == dnsmessage.RCodeNameError,
		}
	}
	return androidTXTRecords(answer[:int(length)])
}

func waitAndroidDNS(ctx context.Context, descriptor int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var cancelPipe [2]int
	if err := unix.Pipe2(cancelPipe[:], unix.O_CLOEXEC); err != nil {
		return fmt.Errorf("creating Android DNS cancellation pipe: %w", err)
	}
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = unix.Close(cancelPipe[1])
		close(cancelDone)
	})
	defer func() {
		if stopCancel() {
			_ = unix.Close(cancelPipe[1])
		} else {
			<-cancelDone
		}
		_ = unix.Close(cancelPipe[0])
	}()
	descriptors := []unix.PollFd{
		{Fd: int32(descriptor), Events: unix.POLLIN},
		{Fd: int32(cancelPipe[0]), Events: unix.POLLIN},
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := unix.Poll(descriptors, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("waiting for Android DNS response: %w", err)
		}
		if ready > 0 {
			if descriptors[0].Revents&unix.POLLNVAL != 0 {
				return unix.EBADF
			}
			return ctx.Err()
		}
	}
}

func androidTXTRecords(answer []byte) ([]string, error) {
	var message dnsmessage.Message
	if err := message.Unpack(answer); err != nil {
		return nil, fmt.Errorf("invalid Android DNS response: %w", err)
	}
	var records []string
	for _, answer := range message.Answers {
		if txt, ok := answer.Body.(*dnsmessage.TXTResource); ok {
			records = append(records, strings.Join(txt.TXT, ""))
		}
	}
	return records, nil
}

func init() {
	resolver, err := madns.NewResolver(madns.WithDefaultResolver(androidDNSResolver{Resolver: net.DefaultResolver}))
	if err != nil {
		logger.Errorf("failed to configure Android DNS resolver: %v", err)
		return
	}
	madns.DefaultResolver = resolver
}
