# Источник каркаса оператора

Каталоги `cmd/operator`, `api/v1alpha1`, `internal/operator` и `config`
происходят из каркаса Kubebuilder. Kubebuilder не запускается прямо здесь: он
создал бы вложенный Go-модуль, собственные Makefile и Dockerfile и файл
`PROJECT`, которые противоречат структуре монорепозитория из `SYSTEM.md`.

Исходные параметры генерации:

| Параметр            | Значение                                    |
| ------------------- | ------------------------------------------- |
| Kubebuilder         | `v4.13.1`                                   |
| controller-runtime  | `v0.23.3` (закреплён Go-плагином Kubebuilder) |
| Kubernetes API      | ветка `1.35`                                |
| domain              | `h3llo-demo.com`                            |
| group / version     | `valkey` / `v1alpha1`                       |
| kind                | `ValkeyInstance`                            |

Команды, которыми получен эталон:

```sh
go run sigs.k8s.io/kubebuilder/v4@v4.13.1 init \
    --domain h3llo-demo.com \
    --repo github.com/RostislavDugin/managed-valkey/operator-scaffold \
    --license none --skip-go-version-check
go run sigs.k8s.io/kubebuilder/v4@v4.13.1 create api \
    --group valkey --version v1alpha1 --kind ValkeyInstance --resource --controller
```

После первоначального переноса источником генерации служат Go-маркеры и
закреплённый `controller-gen`; его вызывает закрытый рецепт `_generate` из
`Justfile`. При обновлении `controller-runtime` эталон генерируется во временном
каталоге заново и сравнивается с текущими файлами вручную.
