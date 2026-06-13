package main

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/cli/sandboxtable"
)

// runTree lists the claims and forks in scope and renders their parent->child
// lineage DAG. A non-empty pool scopes to claims of that pool (and the forks
// descended from them); an empty pool renders every lineage in the namespace
// scope.
func runTree(namespace string, allNamespaces bool, pool string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var claims v1alpha1.SandboxClaimList
	if err := c.List(ctx, &claims, listOpts(namespace, allNamespaces)...); err != nil {
		return fmt.Errorf("list claims: %w", err)
	}
	var forks v1alpha1.SandboxForkList
	if err := c.List(ctx, &forks, listOpts(namespace, allNamespaces)...); err != nil {
		return fmt.Errorf("list forks: %w", err)
	}

	claimItems := claims.Items
	forkItems := forks.Items
	if pool != "" {
		claimItems, forkItems = scopeToPool(claimItems, forkItems, pool)
	}

	roots := sandboxtable.BuildLineage(claimItems, forkItems)
	fmt.Print(sandboxtable.FormatLineage(roots))
	return nil
}

// scopeToPool narrows claims to those of the named pool, then keeps only the
// forks reachable from those claims through the SourceRef chain (a fork of a
// fork of a pool claim stays in scope). It is a transitive-closure walk over the
// fork source refs.
func scopeToPool(claims []v1alpha1.SandboxClaim, forks []v1alpha1.SandboxFork, pool string) ([]v1alpha1.SandboxClaim, []v1alpha1.SandboxFork) {
	inScope := make(map[string]bool)
	keptClaims := claims[:0:0]
	for i := range claims {
		if claims[i].Spec.PoolRef.Name == pool {
			keptClaims = append(keptClaims, claims[i])
			inScope[claims[i].Name] = true
		}
	}

	// Iterate to a fixed point: a fork enters scope when its source is in scope.
	// Repeats until no new fork is added, so multi-level chains are covered.
	keptForks := forks[:0:0]
	added := true
	taken := make(map[string]bool)
	for added {
		added = false
		for i := range forks {
			f := &forks[i]
			if taken[f.Name] {
				continue
			}
			if inScope[f.Spec.SourceRef.Name] {
				keptForks = append(keptForks, *f)
				inScope[f.Name] = true
				taken[f.Name] = true
				added = true
			}
		}
	}
	return keptClaims, keptForks
}
