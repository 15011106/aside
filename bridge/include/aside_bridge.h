#ifndef ASIDE_BRIDGE_H
#define ASIDE_BRIDGE_H

// Implemented in bridge/kakao.swift via @_cdecl and linked statically.
// Takes a JSON request {"action": "...", ...params} and returns a malloc'd
// JSON response {"ok":bool, "result":..., "error":"..."}; release it with
// aside_free.
char *aside_request(const char *request_json);
void aside_free(char *response);

#endif
