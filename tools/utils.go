package tools

import (
	"runtime"
)

func Stack() string {
	var buf [2 << 10]byte
	return string(buf[:runtime.Stack(buf[:], false)])
}

/*
func ReadConfig() error {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("/etc/pgm")     // linux config dir.
	v.AddConfigPath("./demo_sunum") // optionally, look for config in the working dir.
	err := v.ReadInConfig()         // find and read the config file

	v.Get()

	v.OnConfigChange(func(in fsnotify.Event) {

	})
}


*/
