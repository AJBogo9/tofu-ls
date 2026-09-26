// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2024 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mcuadros/go-defaults"
	"github.com/mitchellh/mapstructure"
	"github.com/opentofu/tofu-ls/internal/tofu/datadir"
)

type ExperimentalFeatures struct {
	ValidateOnSave        bool `mapstructure:"validateOnSave"`
	PrefillRequiredFields bool `mapstructure:"prefillRequiredFields"`
}

type ValidationOptions struct {
	EnableEnhancedValidation bool `mapstructure:"enableEnhancedValidation" default:"true"`

	// UnusedSymbols is copied from DiagnosticsOptions, so that it
	// reaches the module jobs together with the validation options.
	UnusedSymbols bool `mapstructure:"-"`
}

type RenameOptions struct {
	// AddMovedBlock appends a moved block when a resource or a module
	// call is renamed, so that OpenTofu does not destroy and recreate it.
	AddMovedBlock bool `mapstructure:"addMovedBlock" default:"true"`
}

type CompletionOptions struct {
	// AddRequiredProviders declares the provider of a completed resource
	// or data source type in required_providers when it is not there yet.
	AddRequiredProviders bool `mapstructure:"addRequiredProviders" default:"true"`
}

type DiagnosticsOptions struct {
	// UnusedSymbols reports variables and locals which nothing references.
	UnusedSymbols bool `mapstructure:"unusedSymbols" default:"true"`
}

type Indexing struct {
	IgnoreDirectoryNames []string `mapstructure:"ignoreDirectoryNames"`
	IgnorePaths          []string `mapstructure:"ignorePaths"`
}

type InlayHints struct {
	// Values shows the statically known value after var and local references.
	Values bool `mapstructure:"values" default:"true"`
	// MaxLength caps the characters of a value hint.
	MaxLength int `mapstructure:"maxLength" default:"40"`
}

type Tofu struct {
	Path        string `mapstructure:"path"`
	Timeout     string `mapstructure:"timeout"`
	LogFilePath string `mapstructure:"logFilePath"`
}

type Options struct {
	CommandPrefix string   `mapstructure:"commandPrefix"`
	Indexing      Indexing `mapstructure:"indexing"`

	// ExperimentalFeatures encapsulates experimental features users can opt into.
	ExperimentalFeatures ExperimentalFeatures `mapstructure:"experimentalFeatures"`

	Validation ValidationOptions `mapstructure:"validation"`

	Rename      RenameOptions      `mapstructure:"rename"`
	Completion  CompletionOptions  `mapstructure:"completion"`
	Diagnostics DiagnosticsOptions `mapstructure:"diagnostics"`
	InlayHints  InlayHints         `mapstructure:"inlayHints"`

	IgnoreSingleFileWarning bool `mapstructure:"ignoreSingleFileWarning"`

	TofuOptions Tofu `mapstructure:"tofu"`

	XLegacyModulePaths          []string `mapstructure:"rootModulePaths"`
	XLegacyExcludeModulePaths   []string `mapstructure:"excludeModulePaths"`
	XLegacyIgnoreDirectoryNames []string `mapstructure:"ignoreDirectoryNames"`
	XLegacyTofuExecPath         string   `mapstructure:"tofuExecPath"`
	XLegacyTofuExecTimeout      string   `mapstructure:"tofuExecTimeout"`
	XLegacyTofuExecLogFilePath  string   `mapstructure:"tofuExecLogFilePath"`
}

func (o *Options) Validate() error {
	if o.TofuOptions.Path != "" {
		path := o.TofuOptions.Path
		if !filepath.IsAbs(path) {
			return fmt.Errorf("expected absolute path for tofu binary, got %q", path)
		}
		stat, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("unable to find tofu binary: %s", err)
		}
		if stat.IsDir() {
			return fmt.Errorf("expected a tofu binary, got a directory: %q", path)
		}
	}

	if len(o.Indexing.IgnoreDirectoryNames) > 0 {
		for _, directory := range o.Indexing.IgnoreDirectoryNames {
			if directory == datadir.DataDirName {
				return fmt.Errorf("cannot ignore directory %q", datadir.DataDirName)
			}

			if strings.Contains(directory, string(filepath.Separator)) {
				return fmt.Errorf("expected directory name, got a path: %q", directory)
			}
		}
	}

	return nil
}

type DecodedOptions struct {
	Options    *Options
	UnusedKeys []string
}

func DecodeOptions(input interface{}) (*DecodedOptions, error) {
	var md mapstructure.Metadata
	options := new(Options)

	// We explicitly set the defaults here before decoding the options.
	// If we were to supply a zero value of a type via our input,
	// setting the default afterwards would override it.
	defaults.SetDefaults(options)

	config := &mapstructure.DecoderConfig{
		Metadata: &md,
		Result:   &options,
	}
	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		panic(err)
	}

	if err := decoder.Decode(input); err != nil {
		return nil, err
	}

	return &DecodedOptions{
		Options:    options,
		UnusedKeys: md.Unused,
	}, nil
}
