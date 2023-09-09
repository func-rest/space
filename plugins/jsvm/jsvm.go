// Package jsvm implements pluggable utilities for binding a JS goja runtime
// to the PocketBase instance (loading migrations, attaching to app hooks, etc.).
//
// Example:
//
//	jsvm.MustRegister(app, jsvm.Config{
//		WatchHooks: true,
//	})
package jsvm

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/console"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/dop251/goja_nodejs/process"
	"github.com/dop251/goja_nodejs/require"
	"github.com/evanw/esbuild/pkg/api"
	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
	"github.com/labstack/echo/v5"

	"github.com/func-rest/space/core"
	m "github.com/func-rest/space/migrations"
	"github.com/func-rest/space/plugins/jsvm/builder/cdn"
	"github.com/func-rest/space/plugins/jsvm/internal/types/generated"
	"github.com/func-rest/space/tools/template"
	"github.com/pocketbase/dbx"
)

const (
	typesFileName = "types.d.ts"
)

//go:embed all:emb/**.ts
var WorkspaceTypes string

//go:embed all:types.d.ts
var EmbedServer embed.FS

type SharedDynObjectState struct {
	sync.RWMutex
	m map[string]goja.Value
}

func (t *SharedDynObjectState) Get(key string) goja.Value {
	t.RLock()
	val := t.m[key]
	t.RUnlock()
	return val
}

func (t *SharedDynObjectState) Set(key string, val goja.Value) bool {
	t.Lock()
	t.m[key] = val
	t.Unlock()
	return true
}

func (t *SharedDynObjectState) Has(key string) bool {
	t.RLock()
	_, exists := t.m[key]
	t.RUnlock()
	return exists
}

func (t *SharedDynObjectState) Delete(key string) bool {
	t.Lock()
	delete(t.m, key)
	t.Unlock()
	return true
}

func (t *SharedDynObjectState) Keys() []string {
	t.RLock()
	keys := make([]string, 0, len(t.m))
	for k := range t.m {
		keys = append(keys, k)
	}
	t.RUnlock()
	return keys
}

var requireRegistry *require.Registry   // = new(require.Registry)
var templateRegistry *template.Registry // = template.NewRegistry()
var shared *SharedDynObjectState

// Config defines the config options of the jsvm plugin.
type Config struct {
	// HooksWatch enables auto app restarts when a JS app hook file changes.
	//
	// Note that currently the application cannot be automatically restarted on Windows
	// because the restart process relies on execve.
	HooksWatch bool

	// HooksDir specifies the JS app hooks directory.
	//
	// If not set it fallbacks to a relative ".data/../.hooks" directory.
	HooksDir string

	// HooksFilesPattern specifies a regular expression pattern that
	// identify which file to load by the hook vm(s).
	//
	// If not set it fallbacks to `^.*(\.pb\.js|\.pb\.ts)$`, aka. any
	// HookdsDir file ending in ".pb.js" or ".pb.ts" (the last one is to enforce IDE linters).
	HooksFilesPattern string

	// HooksPoolSize specifies how many goja.Runtime instances to prewarm
	// and keep for the JS app hooks gorotines execution.
	//
	// Zero or negative value means that it will create a new goja.Runtime
	// on every fired goroutine.
	HooksPoolSize int

	// MigrationsDir specifies the JS migrations directory.
	//
	// If not set it fallbacks to a relative ".data/../pb_migrations" directory.
	MigrationsDir string

	// If not set it fallbacks to `^.*(\.js|\.ts)$`, aka. any MigrationDir file
	// ending in ".js" or ".ts" (the last one is to enforce IDE linters).
	MigrationsFilesPattern string

	// TypesDir specifies the directory where to store the embedded
	// TypeScript declarations file.
	//
	// If not set it fallbacks to ".data".
	TypesDir string

	ServersDir string
}

// MustRegister registers the jsvm plugin in the provided app instance
// and panics if it fails.
//
// Example usage:
//
//	jsvm.MustRegister(app, jsvm.Config{})
func MustRegister(app core.App, config Config) {
	if err := Register(app, config); err != nil {
		panic(err)
	}
}

// Register registers the jsvm plugin in the provided app instance.
func Register(app core.App, config Config) error {
	p := &plugin{app: app, config: config}

	if p.config.HooksDir == "" {
		p.config.HooksDir = filepath.Join(app.DataDir(), "../.hooks")
	}

	if p.config.MigrationsDir == "" {
		p.config.MigrationsDir = filepath.Join(app.DataDir(), "../pb_migrations")
	}

	if p.config.HooksFilesPattern == "" {
		p.config.HooksFilesPattern = `^.*(\.cubby\.js|\.cubby\.ts)$`
	}

	if p.config.MigrationsFilesPattern == "" {
		p.config.MigrationsFilesPattern = `^.*(\.js|\.ts)$`
	}

	if p.config.TypesDir == "" {
		p.config.TypesDir = app.DataDir()
	}

	if p.config.ServersDir == "" {
		p.config.ServersDir = filepath.Join(app.DataDir(), "../.servers")
	}

	if !filepath.IsAbs(p.config.ServersDir) {
		filepath.Abs(p.config.ServersDir)
	}

	p.app.OnAfterBootstrap().Add(func(e *core.BootstrapEvent) error {
		// always update the app types on start to ensure that
		// the user has the latest generated declarations
		if err := p.saveTypesFile(); err != nil {
			color.Yellow("Unable to save app types file: %v", err)
		}

		return nil
	})

	requireRegistry = new(require.Registry)
	templateRegistry = template.NewRegistry()
	shared = &SharedDynObjectState{m: map[string]goja.Value{}}

	if err := p.registerMigrations(); err != nil {
		return fmt.Errorf("registerMigrations: %w", err)
	}

	if err := p.registerHooks(); err != nil {
		return fmt.Errorf("registerHooks: %w", err)
	}

	if err := p.registerServers(); err != nil {
		return fmt.Errorf("registerServers: %w", err)
	}

	return nil
}

type plugin struct {
	app        core.App
	serverCode api.BuildResult
	loops      *loopPool
	config     Config
}

type Request struct {
	goja.Object
}

func RequestFromEcho(c echo.Context, vm *goja.Runtime) goja.Object {
	v := vm.NewObject()
	v.Set("url", c.Request().URL)
	// v.Set("respond", responder)
	v.Set("json", func(call goja.FunctionCall) goja.Value {
		c.JSON(200, call.Arguments[0].Export())
		return nil
	})
	v.Set("html", func(call goja.FunctionCall) goja.Value {
		c.HTML(200, call.Arguments[0].String())
		return nil
	})
	return *v
}

type GojaFn = func(call goja.FunctionCall) goja.Value

func (p *plugin) registerServers() error {

	absServersDir := p.config.ServersDir

	p.serverCode = api.Build(api.BuildOptions{
		// EntryPoints: []string{
		// 	"./",
		// },
		Stdin: &api.StdinOptions{
			Contents: `
			import { config } from './index.cubby.ts';
			
			config()
				.then(res => $configure(res))
				.catch(err => {
					console.log("Here is error", err)
				})
`,
			ResolveDir: absServersDir,
			Sourcefile: "init.ts",
			Loader:     api.LoaderTSX,
		},
		// EntryPointsAdvanced: ent,
		Plugins: []api.Plugin{
			cdn.CDNPlugin,
		},

		AbsWorkingDir: absServersDir,
		Format:        api.FormatIIFE,
		Target:        api.ES2017,
		Engines: []api.Engine{
			{Name: api.EngineNode, Version: "12"},
		},
		Bundle:            true,
		MinifyWhitespace:  false,
		MinifyIdentifiers: false,
		TsconfigRaw: `{
			"compilerOptions": {
				"module": "esnext",
				"moduleResolution": "node",
				"target": "es2017",
				"resolveJsonModule": true,
				"skipLibCheck": true
			},	
		}`,
		// TsconfigRaw: `{
		// 	"compilerOptions": {
		// 		"target": "es2017",
		// 		"allowJs": true,
		// 		"checkJs": true,
		// 		"preserveValueImports": false,
		// 		"esModuleInterop": true,
		// 		"forceConsistentCasingInFileNames": true,
		// 		"resolveJsonModule": true,
		// 		"skipLibCheck": true,
		// 		"sourceMap": true,
		// 		"strict": true
		// 	}
		// }`,
		MinifySyntax: false,
		LogLevel:     api.LogLevelInfo,
		Outdir:       filepath.Join(filepath.Dir(absServersDir), "/.server_build"),
		Write:        true,
	})

	sharedDyn := goja.NewSharedDynamicObject(shared)
	var runnerV *goja.Runtime = goja.New()

	var bindRunner = func(vm *goja.Runtime) {

		requireRegistry.Enable(vm)
		console.Enable(vm)
		process.Enable(vm)
		baseBinds(vm)
		dbxBinds(vm)
		filesystemBinds(vm)
		tokensBinds(vm)
		securityBinds(vm)
		osBinds(vm)
		filepathBinds(vm)
		httpClientBinds(vm)
		formsBinds(vm)
		apisBinds(vm)
		vm.Set("$app", p.app)
		vm.Set("$template", templateRegistry)
		vm.Set("__servers", absServersDir)
		vm.Set("$shared", sharedDyn)
		vm.Set("$configure", func(conf goja.FunctionCall) goja.Value {

			vval := conf.Arguments[0].Export().(map[string]interface{})
			fmt.Println("Configure", vval)
			mountP, found := vval["mountAt"]
			if !found || mountP == nil {
				return nil
			}
			endp, found := vval["endpoints"]

			fmt.Println("configure", vval)

			if found && mountP.(string) != "" && endp != nil {
				p.app.OnBeforeServe().Add(func(e *core.ServeEvent) error {

					handler := func(c echo.Context) error {
						var sk string = strings.Split(c.PathParams().Get("*", "default"), "/")[0]
						var v, fnd = endp.(map[string]interface{})[sk]
						if !fnd {
							c.JSON(404, map[string]interface{}{"errror": "nothing here", "key": sk, "details": c.PathParams()})
							return nil
						}
						var req goja.Object = RequestFromEcho(c, vm)
						var fn GojaFn = v.(GojaFn)
						res := fn(goja.FunctionCall{
							This: vm.NewObject(),
							Arguments: []goja.Value{
								vm.ToValue(req),
							},
						})
						totalResult := res.Export().(*goja.Promise)
						if totalResult.State() == goja.PromiseStateRejected {
							totalErr := totalResult.Result().Export()
							return c.JSON(500, totalErr)
						} else {
							if totalResult.State() == goja.PromiseStateFulfilled {
								totalSucc := totalResult.Result().Export()
								return c.JSON(200, totalSucc)
							} else {
								// totalError := totalResult
								return c.JSON(500, map[string]interface{}{"error": totalResult})
							}
						}
					}
					e.Router.Any(mountP.(string)+"*", handler)
					e.Router.Any(mountP.(string)+"/", handler)
					e.Router.Any(mountP.(string)+"*/", handler)

					return nil
				})
				return nil
			}
			return nil
		})

		_, err := vm.RunString(string(p.serverCode.OutputFiles[0].Contents))
		if err != nil {
			fmt.Println("Error on init", err)
		}

	}
	// ww.Add(1)
	bindRunner(runnerV)
	// runnerV.
	// ww.Wait()
	fmt.Println("Done init")

	// loop := eventloop.NewEventLoop(eventloop.WithRegistry(requireRegistry), eventloop.EnableConsole(true))
	// loop.Start()
	// defer loop.Stop()

	// sigs := make(chan (goja.Value), 1)
	// var w sync.WaitGroup
	// loop.RunOnLoop(func(vm *goja.Runtime) {
	// 	w.Add(1)
	// 	p, resolve, _ := vm.NewPromise()
	// 	vm.Set("p", p)
	// 	go func() {
	// 		// time.Sleep(500 * time.Millisecond)   // or perform any other blocking operation
	// 		loop.RunOnLoop(func(*goja.Runtime) { // resolve() must be called on the loop, cannot call it here
	// 			// resolve(result)
	// 			<-sigs
	// 		})
	// 	}()
	// })

	// w.Wait()
	var w sync.WaitGroup
	p.loops = newLoopPool(100, func() *eventloop.EventLoop {
		n := eventloop.NewEventLoop()
		n.Start()
		defer n.Stop()
		w.Add(1)
		n.RunOnLoop(func(vm *goja.Runtime) {
			requireRegistry.Enable(vm)
			console.Enable(vm)
			process.Enable(vm)
			baseBinds(vm)
			dbxBinds(vm)
			filesystemBinds(vm)
			tokensBinds(vm)
			securityBinds(vm)
			osBinds(vm)
			filepathBinds(vm)
			httpClientBinds(vm)
			formsBinds(vm)
			apisBinds(vm)
			vm.Set("$server", p.serverCode)
			vm.Set("$app", p.app)
			vm.Set("$template", templateRegistry)
			vm.Set("__servers", absServersDir)
			vm.Set("$shared", sharedDyn)
			// vm.Set("$configure", func(conf goja.Value) {
			// 	// fmt.Println("Done@", conf.Export())

			// 	vval := conf.Export().(map[string]interface{})
			// 	// fmt.Println(vval)
			// 	for k, v := range vval {
			// 		fmt.Println(k, v)
			// 	}

			// 	w.Done()
			// })

			// _, err := vm.RunString(string(p.serverCode.OutputFiles[0].Contents))
			// if err != nil {
			// 	fmt.Println("Error on init", err)
			// }
			// pr := res.Export().(goja.Promise)
			// fmt.Println("Result", pr.Result())
			// fmt.Println()
			w.Done()

		})

		return n
	})
	w.Wait()
	fmt.Println("Done init")
	// p.loops.run(func(vm *goja.Runtime) error {
	// 	fmt.Println("Begiiin")
	// 	res, err := vm.RunString(`console.log("__serververs")`)
	// 	fmt.Println("done!", res)
	// 	return err
	// })
	fmt.Println("doint")
	// p.loops.run(func(vm *goja.Runtime) error {
	// 	vm.RunOnLoop(func(r *goja.Runtime) {
	// 		res, err := r.RunString(`console.log(__serververs)`)
	// 		if err != nil {
	// 			fmt.Println("Error on init", err)
	// 		}
	// 		fmt.Println(res)
	// 	})
	// 	return nil
	// })

	return nil
}

// registerMigrations registers the JS migrations loader.
func (p *plugin) registerMigrations() error {
	// fetch all js migrations sorted by their filename
	files, err := filesContent(p.config.MigrationsDir, p.config.MigrationsFilesPattern)
	if err != nil {
		return err
	}

	requireRegistry := new(require.Registry) // this can be shared by multiple runtimes

	for file, content := range files {
		vm := goja.New()
		requireRegistry.Enable(vm)
		console.Enable(vm)
		process.Enable(vm)
		baseBinds(vm)
		dbxBinds(vm)
		tokensBinds(vm)
		securityBinds(vm)
		// note: disallow for now and give the authors of custom SaaS offerings
		// 		 some time to adjust their code to avoid eventual security issues
		//
		// osBinds(vm)
		// filepathBinds(vm)
		// httpClientBinds(vm)

		vm.Set("migrate", func(up, down func(db dbx.Builder) error) {
			m.AppMigrations.Register(up, down, file)
		})

		_, err := vm.RunString(string(content))
		if err != nil {
			return fmt.Errorf("failed to run migration %s: %w", file, err)
		}
	}

	return nil
}

// registerHooks registers the JS app hooks loader.
func (p *plugin) registerHooks() error {
	// fetch all js hooks sorted by their filename
	files, err := filesContent(p.config.HooksDir, p.config.HooksFilesPattern)
	if err != nil {
		return err
	}

	// prepend the types reference directive
	//
	// note: it is loaded during startup to handle conveniently also
	// the case when the HooksWatch option is enabled and the application
	// restart on newly created file
	for name, content := range files {
		if len(content) != 0 {
			// skip non-empty files for now to prevent accidental overwrite
			continue
		}
		path := filepath.Join(p.config.HooksDir, name)
		directive := `/// <reference path="` + p.relativeTypesPath(p.config.HooksDir) + `" />`
		if err := prependToEmptyFile(path, directive+"\n\n"); err != nil {
			color.Yellow("Unable to prepend the types reference: %v", err)
		}
	}

	// initialize the hooks dir watcher
	if p.config.HooksWatch {
		if err := p.watchHooks(); err != nil {
			return err
		}
	}

	if len(files) == 0 {
		// no need to register the vms since there are no entrypoint files anyway
		return nil
	}

	absHooksDir, err := filepath.Abs(p.config.HooksDir)
	if err != nil {
		return err
	}

	p.app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		e.Router.HTTPErrorHandler = p.normalizeServeExceptions(e.Router.HTTPErrorHandler)
		return nil
	})

	// safe to be shared across multiple vms

	sharedBinds := func(vm *goja.Runtime) {
		requireRegistry.Enable(vm)
		console.Enable(vm)
		process.Enable(vm)
		baseBinds(vm)
		dbxBinds(vm)
		filesystemBinds(vm)
		tokensBinds(vm)
		securityBinds(vm)
		osBinds(vm)
		filepathBinds(vm)
		httpClientBinds(vm)
		formsBinds(vm)
		apisBinds(vm)
		vm.Set("$app", p.app)
		vm.Set("$template", templateRegistry)
		vm.Set("__hooks", absHooksDir)
	}

	// initiliaze the executor vms
	executors := newPool(p.config.HooksPoolSize, func() *goja.Runtime {
		executor := goja.New()
		sharedBinds(executor)
		return executor
	})

	// initialize the loader vm
	loader := goja.New()
	sharedBinds(loader)
	hooksBinds(p.app, loader, executors)
	cronBinds(p.app, loader, executors)
	routerBinds(p.app, loader, executors)

	for file, _ := range files {
		func() {
			defer func() {
				if err := recover(); err != nil {
					fmtErr := fmt.Errorf("failed to execute %s:\n - %v", file, err)

					if p.config.HooksWatch {
						color.Red("%v", fmtErr)
					} else {
						panic(fmtErr)
					}
				}
			}()

			lastResult := api.Build(api.BuildOptions{
				EntryPoints: []string{
					"./" + file,
				},
				Plugins: []api.Plugin{
					cdn.CDNPlugin,
				},

				AbsWorkingDir:     absHooksDir,
				Format:            api.FormatIIFE,
				Target:            api.ES2015,
				Bundle:            true,
				MinifyWhitespace:  false,
				MinifyIdentifiers: false,
				TsconfigRaw: `{
					"compilerOptions": {
						"target": "es2017",
						"allowJs": true,
						"checkJs": true,
						"preserveValueImports": false,
						"esModuleInterop": true,
						"forceConsistentCasingInFileNames": true,
						"resolveJsonModule": true,
						"skipLibCheck": true,
						"sourceMap": true,
						"strict": true
					}
				}`,
				MinifySyntax: false,
				LogLevel:     api.LogLevelInfo,
				Outdir:       filepath.Join(filepath.Dir(absHooksDir), "/.hook_build"),
				Write:        false,
			})

			// fmt.Println(lastResult)

			// _, err := loader.RunString(string(content))
			if len(lastResult.OutputFiles) > 0 {
				r, err := loader.RunString(string(lastResult.OutputFiles[0].Contents))
				fmt.Println(r)
				if err != nil {
					// fmt.Println(err)
					panic(err)
					// return err
				}
			} else {
				fmt.Println("Error while building")
			}
		}()
	}

	return nil
}

// normalizeExceptions wraps the provided error handler and returns a new one
// with extracted goja exception error value for consistency when throwing or returning errors.
func (p *plugin) normalizeServeExceptions(oldErrorHandler echo.HTTPErrorHandler) echo.HTTPErrorHandler {
	return func(c echo.Context, err error) {
		defer func() {
			oldErrorHandler(c, err)
		}()

		if err == nil || c.Response().Committed {
			return // no error or already committed
		}

		jsException, ok := err.(*goja.Exception)
		if !ok {
			return // no exception
		}

		switch v := jsException.Value().Export().(type) {
		case error:
			err = v
		case map[string]any: // goja.GoError
			if vErr, ok := v["value"].(error); ok {
				err = vErr
			}
		}
	}
}

// watchHooks initializes a hooks file watcher that will restart the
// application (*if possible) in case of a change in the hooks directory.
//
// This method does nothing if the hooks directory is missing.
func (p *plugin) watchHooks() error {
	if _, err := os.Stat(p.config.HooksDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // no hooks dir to watch
		}
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	var debounceTimer *time.Timer

	stopDebounceTimer := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
			debounceTimer = nil
		}
	}

	p.app.OnTerminate().Add(func(e *core.TerminateEvent) error {
		watcher.Close()

		stopDebounceTimer()

		return nil
	})

	// start listening for events.
	go func() {
		defer stopDebounceTimer()

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				stopDebounceTimer()

				debounceTimer = time.AfterFunc(50*time.Millisecond, func() {
					// app restart is currently not supported on Windows
					if runtime.GOOS == "windows" {
						color.Yellow("File %s changed, please restart the app", event.Name)
					} else {
						color.Yellow("File %s changed, restarting...", event.Name)
						if err := p.app.Restart(); err != nil {
							color.Red("Failed to restart the app:", err)
						}
					}
				})
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				color.Red("Watch error:", err)
			}
		}
	}()

	// add directories to watch
	//
	// @todo replace once recursive watcher is added (https://github.com/fsnotify/fsnotify/issues/18)
	dirsErr := filepath.Walk(p.config.HooksDir, func(path string, info fs.FileInfo, err error) error {
		// ignore hidden directories and node_modules
		if !info.IsDir() || info.Name() == "node_modules" {
			return nil
		}

		return watcher.Add(path)
	})
	dirsSecondErr := filepath.Walk(p.config.ServersDir, func(path string, info fs.FileInfo, err error) error {
		// ignore hidden directories and node_modules
		if !info.IsDir() || info.Name() == "node_modules" {
			return nil
		}

		return watcher.Add(path)
	})

	if dirsErr != nil {
		watcher.Close()
	}
	if dirsSecondErr != nil {
		watcher.Close()
	}

	return dirsErr
}

// fullTypesPathReturns returns the full path to the generated TS file.
func (p *plugin) fullTypesPath() string {
	return filepath.Join(p.config.TypesDir, typesFileName)
}

// relativeTypesPath returns a path to the generated TS file relative
// to the specified basepath.
//
// It fallbacks to the full path if generating the relative path fails.
func (p *plugin) relativeTypesPath(basepath string) string {
	fullPath := p.fullTypesPath()

	rel, err := filepath.Rel(basepath, fullPath)
	if err != nil {
		// fallback to the full path
		rel = fullPath
	}

	return rel
}

// saveTypesFile saves the embedded TS declarations as a file on the disk.
func (p *plugin) saveTypesFile() error {
	fullPath := p.fullTypesPath()

	// ensure that the types directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return err
	}

	// retrieve the types data to write
	data, err := generated.Types.ReadFile(typesFileName)
	if err != nil {
		return err
	}

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return err
	}

	return nil
}

// prependToEmptyFile prepends the specified text to an empty file.
//
// If the file is not empty this method does nothing.
func prependToEmptyFile(path, text string) error {
	info, err := os.Stat(path)

	if err == nil && info.Size() == 0 {
		return os.WriteFile(path, []byte(text), 0644)
	}

	return err
}

// filesContent returns a map with all direct files within the specified dir and their content.
//
// If directory with dirPath is missing or no files matching the pattern were found,
// it returns an empty map and no error.
//
// If pattern is empty string it matches all root files.
func filesContent(dirPath string, pattern string) (map[string][]byte, error) {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string][]byte{}, nil
		}
		return nil, err
	}

	var exp *regexp.Regexp
	if pattern != "" {
		var err error
		if exp, err = regexp.Compile(pattern); err != nil {
			return nil, err
		}
	}

	result := map[string][]byte{}

	for _, f := range files {
		if f.IsDir() || (exp != nil && !exp.MatchString(f.Name())) {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(dirPath, f.Name()))
		if err != nil {
			return nil, err
		}

		result[f.Name()] = raw
	}

	return result, nil
}
