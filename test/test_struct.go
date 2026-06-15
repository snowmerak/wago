package main

//go:generate go run ../main.go

type User struct {
	Name     string
	Age      int
	Active   bool
	Nickname *string
	Profile  Profile
	Address  *Address
	Tags     []string
	Friends  []Friend
	Metadata map[string]string
}

type Profile struct {
	Bio string
}

type Address struct {
	City string
}

type Friend struct {
	Name string
}

//wago:export
func ProcessUser(u User) string {
	return "Processed: " + u.Name
}

func main() {}
