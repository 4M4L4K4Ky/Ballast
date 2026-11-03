// Package proxy implementa el proxy inverso :11435 -> :11434 y la extracción
// de contadores de tokens.
//
// Regla crítica (SPEC.md §2.1): nunca puede bloquear ni alterar una petición.
// Si la escritura de métricas falla, se descarta la métrica y la petición sigue.
package proxy
