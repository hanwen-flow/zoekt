package zoekt

import (
	"fmt"
	"log"
	"regexp"
)

var _ = log.Println

// An expression tree coupled with matches
type matchTree interface {
	// returns whether this matches, and if we are sure.
	matches(known map[matchTree]bool) (match bool, sure bool)

	// clears any per-document state of the matchTree, and prepares for
	// evaluating the given doc
	prepare(nextDoc uint32)
	String() string
}

type andMatchTree struct {
	children []matchTree
}

type orMatchTree struct {
	children []matchTree
}

type notMatchTree struct {
	child matchTree
}

type regexpMatchTree struct {
	query  *RegexpQuery
	regexp *regexp.Regexp
	child  matchTree

	// mutable
	reEvaluated bool
	found       []*candidateMatch
}

type substrMatchTree struct {
	query *SubstringQuery

	cands         []*candidateMatch
	coversContent bool

	// mutable
	current       []*candidateMatch
	caseEvaluated bool
	contEvaluated bool
}

type branchQueryMatchTree struct {
	fileMasks []uint32
	mask      uint32

	// mutable
	docID uint32
}

// prepare
func (t *andMatchTree) prepare(doc uint32) {
	for _, c := range t.children {
		c.prepare(doc)
	}
}

func (t *regexpMatchTree) prepare(doc uint32) {
	t.found = t.found[:0]
	t.reEvaluated = false
	t.child.prepare(doc)
}

func (t *orMatchTree) prepare(doc uint32) {
	for _, c := range t.children {
		c.prepare(doc)
	}
}

func (t *notMatchTree) prepare(doc uint32) {
	t.child.prepare(doc)
}

func (t *substrMatchTree) prepare(nextDoc uint32) {
	for len(t.cands) > 0 && t.cands[0].file < nextDoc {
		t.cands = t.cands[1:]
	}

	i := 0
	for ; i < len(t.cands) && t.cands[i].file == nextDoc; i++ {
	}
	t.current = t.cands[:i]
	t.cands = t.cands[i:]
	t.contEvaluated = false
	t.caseEvaluated = false
}

func (t *branchQueryMatchTree) prepare(doc uint32) {
	t.docID = doc
}

// String.
func (t *andMatchTree) String() string {
	return fmt.Sprintf("and%v", t.children)
}

func (t *regexpMatchTree) String() string {
	return fmt.Sprintf("re(%s,%s)", t.regexp, t.child)
}

func (t *orMatchTree) String() string {
	return fmt.Sprintf("or%v", t.children)
}

func (t *notMatchTree) String() string {
	return fmt.Sprintf("not(%v)", t.child)
}

func (t *substrMatchTree) String() string {
	return fmt.Sprintf("substr(%s, %v)", t.query, t.current)
}

func (t *branchQueryMatchTree) String() string {
	return fmt.Sprintf("branch(%x)", t.mask)
}

func collectPositiveSubstrings(t matchTree, f func(*substrMatchTree)) {
	switch s := t.(type) {
	case *andMatchTree:
		for _, ch := range s.children {
			collectPositiveSubstrings(ch, f)
		}
	case *orMatchTree:
		for _, ch := range s.children {
			collectPositiveSubstrings(ch, f)
		}
	case *regexpMatchTree:
		collectPositiveSubstrings(s.child, f)
	case *notMatchTree:
	case *substrMatchTree:
		f(s)
	}
}

func collectRegexps(t matchTree, f func(*regexpMatchTree)) {
	switch s := t.(type) {
	case *andMatchTree:
		for _, ch := range s.children {
			collectRegexps(ch, f)
		}
	case *orMatchTree:
		for _, ch := range s.children {
			collectRegexps(ch, f)
		}
	case *regexpMatchTree:
		f(s)
	}
}

func visitMatches(t matchTree, known map[matchTree]bool, f func(matchTree)) {
	switch s := t.(type) {
	case *andMatchTree:
		for _, ch := range s.children {
			if known[ch] {
				visitMatches(ch, known, f)
			}
		}
	case *orMatchTree:
		for _, ch := range s.children {
			if known[ch] {
				visitMatches(ch, known, f)
			}
		}
	case *notMatchTree:
		// don't collect into negative trees.
	default:
		f(s)
	}
}

func visitSubtreeMatches(t matchTree, known map[matchTree]bool, f func(*substrMatchTree)) {
	visitMatches(t, known, func(mt matchTree) {
		st, ok := mt.(*substrMatchTree)
		if ok {
			f(st)
		}
	})
}

func visitRegexMatches(t matchTree, known map[matchTree]bool, f func(*regexpMatchTree)) {
	visitMatches(t, known, func(mt matchTree) {
		st, ok := mt.(*regexpMatchTree)
		if ok {
			f(st)
		}
	})
}

func (p *contentProvider) evalContentMatches(s *substrMatchTree) {
	if !s.coversContent {
		pruned := s.current[:0]
		for _, m := range s.current {
			if p.matchContent(m) {
				pruned = append(pruned, m)
			}
		}
		s.current = pruned
	}
	s.contEvaluated = true
}

func (p *contentProvider) evalRegexpMatches(s *regexpMatchTree) {
	idxs := s.regexp.FindAllIndex(p.data(false), -1)
	for _, idx := range idxs {
		s.found = append(s.found, &candidateMatch{
			offset:  uint32(idx[0]),
			matchSz: uint32(idx[1] - idx[0]),
		})
	}
	s.reEvaluated = true
}

func (p *contentProvider) evalCaseMatches(s *substrMatchTree) {
	if s.query.CaseSensitive {
		pruned := s.current[:0]
		for _, m := range s.current {
			if p.caseMatches(m) {
				pruned = append(pruned, m)
			}
		}
		s.current = pruned
	}
	s.caseEvaluated = true
}

func (t *andMatchTree) matches(known map[matchTree]bool) (bool, bool) {
	sure := true

	for _, ch := range t.children {
		v, ok := evalMatchTree(known, ch)
		if ok && !v {
			return false, true
		}
		if !ok {
			sure = false
		}
	}
	return true, sure
}

func (t *orMatchTree) matches(known map[matchTree]bool) (bool, bool) {
	sure := true
	for _, ch := range t.children {
		v, ok := evalMatchTree(known, ch)
		if ok {
			if v {
				return true, true
			}
		} else {
			sure = false
		}
	}
	return false, sure
}

func (t *branchQueryMatchTree) matches(known map[matchTree]bool) (bool, bool) {
	return t.fileMasks[t.docID]&t.mask != 0, true
}

func (t *regexpMatchTree) matches(known map[matchTree]bool) (bool, bool) {
	v, ok := evalMatchTree(known, t.child)
	if ok && !v {
		return false, true
	}

	if !t.reEvaluated {
		return false, false
	}

	return len(t.found) > 0, true
}

func evalMatchTree(known map[matchTree]bool, mt matchTree) (bool, bool) {
	if v, ok := known[mt]; ok {
		return v, true
	}

	v, ok := mt.matches(known)
	if ok {
		known[mt] = v
	}

	return v, ok
}

func (t *notMatchTree) matches(known map[matchTree]bool) (bool, bool) {
	v, ok := evalMatchTree(known, t.child)
	return !v, ok
}

func (t *substrMatchTree) matches(known map[matchTree]bool) (bool, bool) {
	if len(t.current) == 0 {
		return false, true
	}

	sure := (!t.query.CaseSensitive || t.caseEvaluated) && (t.coversContent || t.contEvaluated)
	return true, sure
}

func (d *indexData) newMatchTree(q Query, sq map[*SubstringQuery]*substrMatchTree) (matchTree, error) {
	switch s := q.(type) {
	case *RegexpQuery:
		subQ := regexpToQuery(s.Regexp)
		subMT, err := d.newMatchTree(subQ, sq)
		if err != nil {
			return nil, err
		}

		return &regexpMatchTree{
			regexp: regexp.MustCompile(s.Regexp.String()),
			child:  subMT,
		}, nil
	case *AndQuery:
		var r []matchTree
		for _, ch := range s.Children {
			ct, err := d.newMatchTree(ch, sq)
			if err != nil {
				return nil, err
			}
			r = append(r, ct)
		}
		return &andMatchTree{r}, nil
	case *OrQuery:
		var r []matchTree
		for _, ch := range s.Children {
			ct, err := d.newMatchTree(ch, sq)
			if err != nil {
				return nil, err
			}
			r = append(r, ct)
		}
		return &andMatchTree{r}, nil
	case *NotQuery:
		ct, err := d.newMatchTree(s.Child, sq)
		return &notMatchTree{
			child: ct,
		}, err
	case *SubstringQuery:
		iter, err := d.getDocIterator(s)
		if err != nil {
			return nil, err
		}
		st := &substrMatchTree{
			query:         s,
			coversContent: iter.coversContent,
			cands:         iter.next(),
		}
		sq[s] = st
		return st, nil

	case *BranchQuery:
		return &branchQueryMatchTree{
			mask:      uint32(d.branchIDs[s.Name]),
			fileMasks: d.fileBranchMasks,
		}, nil
	}
	panic("type")
	return nil, nil
}

func (d *indexData) Search(q Query) (*SearchResult, error) {
	var res SearchResult

	atoms := map[*SubstringQuery]*substrMatchTree{}
	mt, err := d.newMatchTree(q, atoms)
	if err != nil {
		return nil, err
	}

	for _, st := range atoms {
		res.Stats.NgramMatches += len(st.cands)
	}

	var positiveAtoms, fileAtoms []*substrMatchTree
	collectPositiveSubstrings(mt, func(sq *substrMatchTree) {
		positiveAtoms = append(positiveAtoms, sq)
	})

	var regexpAtoms []*regexpMatchTree
	collectRegexps(mt, func(re *regexpMatchTree) {
		regexpAtoms = append(regexpAtoms, re)
	})

	for _, st := range atoms {
		if st.query.FileName {
			fileAtoms = append(fileAtoms, st)
		}
	}

nextFileMatch:
	for {
		var nextDoc uint32
		nextDoc = maxUInt32
		for _, st := range positiveAtoms {
			if len(st.cands) > 0 && st.cands[0].file < nextDoc {
				nextDoc = st.cands[0].file
			}
		}

		if nextDoc == maxUInt32 {
			break
		}

		res.Stats.FilesConsidered++
		mt.prepare(nextDoc)

		var fileStart uint32
		if nextDoc > 0 {
			fileStart = d.fileEnds[nextDoc-1]
		}
		cp := contentProvider{
			reader:   d.reader,
			id:       d,
			idx:      nextDoc,
			stats:    &res.Stats,
			fileSize: d.fileEnds[nextDoc] - fileStart,
		}

		known := make(map[matchTree]bool)
		if v, ok := evalMatchTree(known, mt); ok && !v {
			continue nextFileMatch
		}

		// Files are cheap to match. Do them first.
		if len(fileAtoms) > 0 {
			for _, st := range fileAtoms {
				cp.evalCaseMatches(st)
				cp.evalContentMatches(st)
			}
			if v, ok := evalMatchTree(known, mt); ok && !v {
				continue nextFileMatch
			}
		}

		for _, st := range atoms {
			cp.evalCaseMatches(st)
		}

		if v, ok := evalMatchTree(known, mt); ok && !v {
			continue nextFileMatch
		}

		for _, st := range atoms {
			// TODO - this may evaluate too much.
			cp.evalContentMatches(st)
		}

		if len(regexpAtoms) > 0 {
			if v, ok := evalMatchTree(known, mt); ok && !v {
				continue nextFileMatch
			}

			for _, re := range regexpAtoms {
				cp.evalRegexpMatches(re)
			}
		}

		if v, ok := evalMatchTree(known, mt); !ok {
			panic("did not decide")
		} else if !v {
			continue nextFileMatch
		}

		fileMatch := FileMatch{
			Name: d.fileName(nextDoc),
			// Maintain ordering of input files. This
			// strictly dominates the in-file ordering of
			// the matches.
			Score: 10 * float64(nextDoc) / float64(len(d.boundaries)),
		}

		foundContentMatch := false
		visitSubtreeMatches(mt, known, func(s *substrMatchTree) {
			for _, c := range s.current {
				fileMatch.Matches = append(fileMatch.Matches, cp.fillMatch(c))
				if !c.fileName {
					foundContentMatch = true
				}
			}
		})

		visitRegexMatches(mt, known, func(re *regexpMatchTree) {
			for _, c := range re.found {
				foundContentMatch = true
				fileMatch.Matches = append(fileMatch.Matches, cp.fillMatch(c))
			}
		})

		if foundContentMatch {
			trimmed := fileMatch.Matches[:0]
			for _, m := range fileMatch.Matches {
				if !m.FileName {
					trimmed = append(trimmed, m)
				}
			}
			fileMatch.Matches = trimmed
		}

		maxFileScore := 0.0
		for i := range fileMatch.Matches {
			if maxFileScore < fileMatch.Matches[i].Score {
				maxFileScore = fileMatch.Matches[i].Score
			}

			// Order by ordering in file.
			fileMatch.Matches[i].Score += 1.0 - (float64(i) / float64(len(fileMatch.Matches)))
		}
		fileMatch.Score += maxFileScore

		foundBranchQuery := false
		visitMatches(mt, known, func(mt matchTree) {
			bq, ok := mt.(*branchQueryMatchTree)
			if ok {
				foundBranchQuery = true
				fileMatch.Branches = append(fileMatch.Branches,
					d.branchNames[int(bq.mask)])
			}
		})

		if !foundBranchQuery {
			mask := d.fileBranchMasks[nextDoc]
			id := uint32(1)
			for mask != 0 {
				if mask&0x1 != 0 {
					fileMatch.Branches = append(fileMatch.Branches, d.branchNames[int(id)])
				}
				id <<= 1
				mask >>= 1
			}
		}

		sortMatchesByScore(fileMatch.Matches)
		res.Files = append(res.Files, fileMatch)
		res.Stats.MatchCount += len(fileMatch.Matches)
		res.Stats.FileCount++
	}
	sortFilesByScore(res.Files)
	return &res, nil
}

func extractSubstringQueries(q Query) []*SubstringQuery {
	var r []*SubstringQuery
	switch s := q.(type) {
	case *AndQuery:
		for _, ch := range s.Children {
			r = append(r, extractSubstringQueries(ch)...)
		}
	case *OrQuery:
		for _, ch := range s.Children {
			r = append(r, extractSubstringQueries(ch)...)
		}
	case *NotQuery:
		r = append(r, extractSubstringQueries(s.Child)...)
	case *SubstringQuery:
		r = append(r, s)
	}
	return r
}