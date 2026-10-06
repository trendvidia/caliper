package usesbody

import "failedpkgs/bodyerr"

func Use() int { return bodyerr.Good() }
