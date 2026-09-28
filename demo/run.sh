#!/usr/bin/env bash
# Демо mk8s-calc по шагам (CLI-часть). Запуск из корня репозитория:
#   ./demo/run.sh          — все шаги подряд с паузами
#   ./demo/run.sh 3        — только шаг 3
# Веб-UI поднимается отдельно: ./mk8s-calc -ui=127.0.0.1:8080
set -euo pipefail
cd "$(dirname "$0")/.."

DUMP=demo/dump.json
OUT=demo/out

go build -o mk8s-calc .

step() {
	echo
	echo "════════════════════════════════════════════════════════════════"
	echo "  Шаг $1. $2"
	echo "════════════════════════════════════════════════════════════════"
}

pause() {
	if [[ -z "${ONLY:-}" && -t 0 ]]; then
		read -r -p "  ⏎ дальше " _
	fi
}

run() {
	local n=$1
	[[ -n "${ONLY:-}" && "$ONLY" != "$n" ]] && return 0
	case $n in
	1)
		step 1 "Базовый расчёт ПРОМ по методике, без override"
		./mk8s-calc -source=paste -env=prom < "$DUMP" 2>/dev/null
		;;
	2)
		step 2 "auto: калькулятор сам подбирает интенсивность override по каждому стенду"
		./mk8s-calc -source=paste -override=auto < "$DUMP" 2>/dev/null
		;;
	3)
		step 3 "Данные сняты с ПСИ, а не с ПРОМ: реплики пересчитываются от источника"
		./mk8s-calc -source=paste -from=psi -env=prom -override=auto < "$DUMP" 2>/dev/null |
			grep -E "Подов|V:|Итог|override auto|Нод:"
		echo
		echo "  search (Java, -Xmx6g) — memory limit в манифесте при hard:"
		rm -rf "$OUT"
		./mk8s-calc -source=paste -from=psi -env=prom -override=auto -manifests="$OUT" < "$DUMP" >/dev/null 2>&1
		python3 -c "import json; c=json.load(open('$OUT/prom/deployment/shop-search.yaml'))['spec']['template']['spec']['containers'][0]; print('   ', c['resources'], '| JAVA_OPTS =', c['env'][0]['value'])"
		;;
	4)
		step 4 "Генерация манифестов под все стенды"
		rm -rf "$OUT"
		./mk8s-calc -source=paste -override=auto -manifests="$OUT" < "$DUMP" 2>/dev/null | sed -n '/Манифесты/,$p'
		echo
		find "$OUT/prom" -type f | sort
		echo
		echo "  orders-api: DeploymentConfig → Deployment, limits сжаты, исходные — в аннотации:"
		python3 -c "import json; o=json.load(open('$OUT/prom/deployment/shop-orders-api.yaml')); print('    kind:', o['kind']); print('    annotations:', o['metadata']['annotations']); print('    resources:', o['spec']['template']['spec']['containers'][0]['resources'])"
		;;
	5)
		step 5 "JSON-план для агента"
		./mk8s-calc -source=paste -env=prom -override=auto -json < "$DUMP" 2>/dev/null | python3 -c "
import json, sys
r = json.load(sys.stdin)
e = r['envs'][0]
print('  schema_version:', r['schema_version'], '| methodology:', r['methodology']['name'], 'v' + r['methodology']['version'])
print('  basis:', r['basis']['metric'], '| usage_metrics:', r['basis']['usage_metrics'])
print('  рекомендация:', e['override_recommendation']['level'], '—', e['override_recommendation']['reason'])
for c in e['override_recommendation']['candidates']:
    print('    %-7s nodes=%d cores=%d ram=%dGiB grows=%s' % (c['level'], c['nodes'], c['nominal_cores'], c['nominal_ram_gib'], c['cluster_grows']))
print('  эффект, ноды:', e['override_effect']['nodes'])
print('  предупреждений по данным:', len(r['data_warnings']))
"
		;;
	esac
	pause
}

if [[ $# -gt 0 ]]; then
	ONLY=$1 run "$1"
else
	for n in 1 2 3 4 5; do run "$n"; done
fi
