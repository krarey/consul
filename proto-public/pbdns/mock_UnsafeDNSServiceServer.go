// Code generated by mockery v2.15.0. DO NOT EDIT.

package pbdns

import mock "github.com/stretchr/testify/mock"

// MockUnsafeDNSServiceServer is an autogenerated mock type for the UnsafeDNSServiceServer type
type MockUnsafeDNSServiceServer struct {
	mock.Mock
}

// mustEmbedUnimplementedDNSServiceServer provides a mock function with given fields:
func (_m *MockUnsafeDNSServiceServer) mustEmbedUnimplementedDNSServiceServer() {
	_m.Called()
}

type mockConstructorTestingTNewMockUnsafeDNSServiceServer interface {
	mock.TestingT
	Cleanup(func())
}

// NewMockUnsafeDNSServiceServer creates a new instance of MockUnsafeDNSServiceServer. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewMockUnsafeDNSServiceServer(t mockConstructorTestingTNewMockUnsafeDNSServiceServer) *MockUnsafeDNSServiceServer {
	mock := &MockUnsafeDNSServiceServer{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
