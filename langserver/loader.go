package langserver

import (
	"context"
	"log"
	"os"
	"runtime/pprof"
	"time"

	"github.com/adamfaulkner/go-langserver/gotype"

	"github.com/adamfaulkner/go-langserver/pkg/lsp"
	"github.com/sourcegraph/jsonrpc2"
)

// Cancel any ongoing operations, create a new context, return it.
func (h *LangHandler) updateContext() context.Context {
	h.adamfMutex.Lock()
	if h.cancelOngoingOperations != nil {
		h.cancelOngoingOperations()
	}
	realCtx, cancel := context.WithCancel(context.Background())
	h.cancelOngoingOperations = cancel
	h.adamfMutex.Unlock()
	return realCtx
}

// TODO(adamf): Split this into several functions. Diagnostics should be
// happening asynchronously and should not have a context.
// Typecheck the document referred to by fileURI. Send diagnostics as appropriate.
func (h *LangHandler) adamfDiagnostics(ctx context.Context, conn jsonrpc2.JSONRPC2, fileURI lsp.DocumentURI) {
	if !isFileURI(fileURI) {
		log.Println("Invalid File URI:", fileURI)
	}
	origFilename := h.FilePath(fileURI)

	profileFile, err := os.Create("/tmp/profile.pprof")
	start := time.Now()
	if err != nil {
		log.Println("error making profile", err)
		return
	}
	defer func() {
		log.Println("Total time", time.Since(start))
		err := pprof.WriteHeapProfile(profileFile)
		if err != nil {
			log.Println("error writing heap profile", err)
		}
	}()

	realCtx := h.updateContext()
	bctx := h.BuildContext(realCtx)
	// cgo is not supported.
	bctx.CgoEnabled = true
	errs := gotype.CheckFile(realCtx, origFilename, bctx)

	diags, err := errsToDiagnostics(errs)
	if err != nil {
		log.Println("Error converting err to diagnostic", err)
		return
	}

	// Make sure that origFilename is represented to cover the case where the
	// final error was just fixed in this file.
	//
	// TODO(adamf): This doesn't really cover all cases where there was
	// previously an error. For example, we can fix other packages to now
	// successfully compile. It would be better to integrate this with the
	// caching/state tracking mechanism.
	if _, ok := diags[origFilename]; !ok {
		diags[origFilename] = nil
	}

	// Do not send diagnostics if our context has since expired.
	if realCtx.Err() != nil {
		log.Println("Context expired")
		return
	}

	if err := h.publishAdamfDiagnostics(realCtx, conn, diags); err != nil {
		log.Printf("warning: failed to send diagnostics: %s.", err)
	}
}
