//go:build !faultinject

package fxapp

import "go.uber.org/fx"

var faultInjection = fx.Options()
