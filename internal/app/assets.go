package app

import "embed"

//go:embed web/*
var webAssets embed.FS

//go:embed parameters.mjs
var parameterPlugin []byte
