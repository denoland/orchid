module orch

go 1.25.0

require (
	github.com/coder/websocket v1.8.14
	github.com/divy/orchid/cfrelaytun/go/cfrelaytun v0.0.0-00010101000000-000000000000
	github.com/hashicorp/hcl/v2 v2.20.1
	github.com/zclconf/go-cty v1.13.0
	modernc.org/sqlite v1.50.0
)

replace github.com/divy/orchid/cfrelaytun/go/cfrelaytun => ./cfrelaytun/go/cfrelaytun

require (
	github.com/agext/levenshtein v1.2.1 // indirect
	github.com/apparentlymart/go-textseg/v13 v13.0.0 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/go-cmp v0.6.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/go-wordwrap v0.0.0-20150314170334-ad45545899c7 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/mod v0.33.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.11.0 // indirect
	golang.org/x/tools v0.42.0 // indirect
	modernc.org/libc v1.72.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
