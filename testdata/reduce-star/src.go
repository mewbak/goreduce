package crasher

func Crasher() {
	a := []*int{}
	_ = a[0]
}