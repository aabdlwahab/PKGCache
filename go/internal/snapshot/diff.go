package snapshot

import "fmt"

// Change is one entry-level difference between two manifests. A removal has Target
// nil; an addition has Base nil; a replacement has both.
type Change struct {
	Base   *Entry
	Target *Entry
}

// Diff performs a linear merge of two sorted manifests.
func Diff(base, target *Iterator, yield func(Change) error) error {
	left, lok, err := base.Next()
	if err != nil {
		return err
	}
	right, rok, err := target.Next()
	if err != nil {
		return err
	}
	for lok || rok {
		switch {
		case !rok || lok && entryOrder(left, right) < 0:
			// Copied so the yielded pointer does not alias the iterator's cursor.
			removed := left
			if err := yield(Change{Base: &removed}); err != nil {
				return err
			}
			left, lok, err = base.Next()
		case !lok || entryOrder(left, right) > 0:
			added := right
			if err := yield(Change{Target: &added}); err != nil {
				return err
			}
			right, rok, err = target.Next()
		default:
			if left.Digest != right.Digest || left.Size != right.Size {
				lcopy, rcopy := left, right
				if err := yield(Change{Base: &lcopy, Target: &rcopy}); err != nil {
					return err
				}
			}
			left, lok, err = base.Next()
			if err == nil {
				right, rok, err = target.Next()
			}
		}
		if err != nil {
			return fmt.Errorf("snapshot: diff: %w", err)
		}
	}
	return nil
}

func entryOrder(a, b Entry) int {
	if a.Eco < b.Eco {
		return -1
	}
	if a.Eco > b.Eco {
		return 1
	}
	if a.Key < b.Key {
		return -1
	}
	if a.Key > b.Key {
		return 1
	}
	return 0
}
