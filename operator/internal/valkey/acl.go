package valkey

import (
	"fmt"
)

const appPermissions = "~* &* -@all +@read +@write +@pubsub +@transaction +@keyspace +@string +@list +@hash +@set +@sortedset +@stream +@bitmap +@hyperloglog +@geo +@blocking +@connection +@scripting -@dangerous +ping +echo +select +info +flushdb +flushall +sort +sort_ro"

func InitialACL(appPasswordHash, operatorPassword, replicaPassword, healthPassword string) []byte {
	return []byte(fmt.Sprintf(
		"user default off\n"+
			"user app off #%s %s\n"+
			"user operator on >%s ~* &* +@all\n"+
			"user replica on >%s -@all +psync +replconf +ping\n"+
			"user health on >%s -@all +ping +role +info\n",
		appPasswordHash,
		appPermissions,
		operatorPassword,
		replicaPassword,
		healthPassword,
	))
}
