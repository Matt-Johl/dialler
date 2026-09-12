# Dialler replacement for baresip's modules/g722/CMakeLists.txt (see
# ios/vendor/patches/README.md). Upstream needs spandsp (LGPL), which the
# app must not link statically; this builds the module against WebRTC's
# copy of Steve Underwood's public-domain G.722 (webrtc/modules/third_party/
# g722, BSD-3 for WebRTC's edits), compiled into the module itself.
project(g722)

list(APPEND MODULES_DETECTED ${PROJECT_NAME})
set(MODULES_DETECTED ${MODULES_DETECTED} PARENT_SCOPE)

set(SRCS
  g722.c
  webrtc/modules/third_party/g722/g722_encode.c
  webrtc/modules/third_party/g722/g722_decode.c
)

if(STATIC)
    add_library(${PROJECT_NAME} OBJECT ${SRCS})
else()
    add_library(${PROJECT_NAME} MODULE ${SRCS})
endif()

target_include_directories(${PROJECT_NAME} PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/webrtc)
