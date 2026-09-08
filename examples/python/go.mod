module github.com/adrianliechti/shale/examples/python

go 1.27.1

require (
	github.com/adrianliechti/go-pyodide v0.0.0-20260905215709-0141242c74c6
	github.com/adrianliechti/shale v0.0.0
)

require (
	github.com/tetratelabs/wazero v1.12.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	mvdan.cc/sh/v3 v3.14.0 // indirect
)

replace github.com/adrianliechti/shale => ../..

replace github.com/adrianliechti/go-pyodide => ../../../go-pyodide
