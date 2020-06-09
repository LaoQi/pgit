package pgs

type Config struct {

}

var config *Config

func InitConfig() *Config {
	config = &Config{}
	return config
}