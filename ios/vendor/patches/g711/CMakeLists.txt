# Dialler replacement for baresip's modules/g711/CMakeLists.txt: upstream's
# module plus packet-loss concealment (plc/, shared with g722; see
# ios/vendor/patches/README.md).
project(g711)

list(APPEND MODULES_DETECTED ${PROJECT_NAME})
set(MODULES_DETECTED ${MODULES_DETECTED} PARENT_SCOPE)

set(SRCS g711.c plc/plc.c)

if(STATIC)
    add_library(${PROJECT_NAME} OBJECT ${SRCS})
else()
    add_library(${PROJECT_NAME} MODULE ${SRCS})
endif()

target_include_directories(${PROJECT_NAME} PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/plc)
