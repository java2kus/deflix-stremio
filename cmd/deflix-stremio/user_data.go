package main

// TODO: having v1 and v2 structs here doesn't help, we can only register one in go-stremio.
//		 So either we make future changes backward-compatible, or go-stremio must support versioning itself.
//		 go-stremio could take a map that maps versions to types, and then assumes that the data always has a "v" field to determine which type to pick.
//		 Also: How do we deal with non-base64 api tokens vs base64 userdata structs?
//		  - either turn this userdata into url-safe json, but no base64
//		  - or split traffic via regex to two different containers: an old one with only RD support and the new one. but old users would then miss features, and how do we get them to learn that they have to "reinstall" the addon?
//		  - or make the decoding fully customizable by allowing to set something like a `func(data string) (interface{}, error)`
type userData struct {
	// RealDebrid
	RDtoken  string `json:"rdToken"`
	RDremote bool   `json:"rdRemote"`
	// AllDebrid
	ADkey string `json:"adKey"`
}
